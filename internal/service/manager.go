package service

import (
	"context"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

var log = logf.Log.WithName("service-manager")

// Manager reconciles a Kubernetes Service and EndpointSlice for a FailoverService.
type Manager struct {
	Client      client.Client
	ClusterRole string // "primary" or "failover"
}

// ResolveActiveAddress returns the address to use for the managed Service based on
// the current cluster role and failover state.
//
// Each cluster target has two addresses:
//   - PrimaryModeAddress: used when service is in primary mode
//   - FailoverModeAddress: used when service is in failover mode
//
// The controller uses CLUSTER_ROLE to select the target (primaryCluster or failoverCluster),
// then uses failoverActive to select the address.
func (m *Manager) ResolveActiveAddress(fs *failoverv1alpha1.FailoverService) string {
	return m.ResolveActiveAddressWithDirection(fs, fs.Spec.FailoverActive)
}

// ResolveActiveAddressWithDirection returns the address to use for the managed Service
// using an explicit failoverActive value rather than reading from spec.
func (m *Manager) ResolveActiveAddressWithDirection(fs *failoverv1alpha1.FailoverService, failoverActive bool) string {
	var target *failoverv1alpha1.ClusterTarget
	if m.ClusterRole == "primary" {
		target = &fs.Spec.PrimaryCluster
	} else {
		target = &fs.Spec.FailoverCluster
	}

	if failoverActive {
		return target.FailoverModeAddress
	}
	return target.PrimaryModeAddress
}

// Reconcile ensures the managed Service and optional EndpointSlice match the desired state.
func (m *Manager) Reconcile(ctx context.Context, fs *failoverv1alpha1.FailoverService) error {
	return m.ReconcileWithDirection(ctx, fs, fs.Spec.FailoverActive)
}

// ReconcileWithDirection ensures the managed Service and optional EndpointSlice match
// the desired state using an explicit failoverActive value rather than reading from spec.
func (m *Manager) ReconcileWithDirection(ctx context.Context, fs *failoverv1alpha1.FailoverService, failoverActive bool) error {
	address := m.ResolveActiveAddressWithDirection(fs, failoverActive)
	return m.ReconcileWithAddress(ctx, fs, address)
}

// ReconcileWithAddress ensures the managed Service and optional EndpointSlice match
// the desired state using an explicit target address snapshot.
func (m *Manager) ReconcileWithAddress(ctx context.Context, fs *failoverv1alpha1.FailoverService, address string) error {
	isIP := net.ParseIP(address) != nil

	if isIP {
		return m.reconcileClusterIPService(ctx, fs, address)
	}
	return m.reconcileExternalNameService(ctx, fs, address)
}

func (m *Manager) reconcileExternalNameService(ctx context.Context, fs *failoverv1alpha1.FailoverService, dnsName string) error {
	svcName := fs.Spec.ServiceName
	ns := fs.Namespace

	existing := &corev1.Service{}
	err := m.Client.Get(ctx, types.NamespacedName{Name: svcName, Namespace: ns}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("getting service: %w", err)
	}

	desired := m.buildExternalNameService(fs, dnsName)

	if errors.IsNotFound(err) {
		log.Info("Creating ExternalName service", "name", svcName, "namespace", ns, "target", dnsName)
		return m.Client.Create(ctx, desired)
	}

	// Refuse to modify a Service we don't own
	if !isOwnedBy(existing.OwnerReferences, fs) {
		return fmt.Errorf("service %s/%s exists but is not owned by FailoverService %s; refusing to modify", ns, svcName, fs.Name)
	}

	// If the existing service is already ExternalName, just update it
	if existing.Spec.Type == corev1.ServiceTypeExternalName {
		needsUpdate := existing.Spec.ExternalName != dnsName
		if !needsUpdate {
			return nil // already correct
		}
		existing.Spec.ExternalName = dnsName
		existing.Spec.Ports = nil // ExternalName services don't use ports
		return m.Client.Update(ctx, existing)
	}

	// Type change required (ClusterIP -> ExternalName): delete and recreate
	log.Info("Switching service type to ExternalName", "name", svcName, "namespace", ns)
	if err := m.deleteEndpointSlice(ctx, fs); err != nil {
		return err
	}
	if err := m.Client.Delete(ctx, existing); err != nil {
		return fmt.Errorf("deleting old service: %w", err)
	}
	return m.Client.Create(ctx, desired)
}

func (m *Manager) reconcileClusterIPService(ctx context.Context, fs *failoverv1alpha1.FailoverService, ip string) error {
	svcName := fs.Spec.ServiceName
	ns := fs.Namespace

	existing := &corev1.Service{}
	err := m.Client.Get(ctx, types.NamespacedName{Name: svcName, Namespace: ns}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("getting service: %w", err)
	}

	desired := m.buildClusterIPService(fs)

	if errors.IsNotFound(err) {
		log.Info("Creating ClusterIP service", "name", svcName, "namespace", ns, "target", ip)
		if err := m.Client.Create(ctx, desired); err != nil {
			return err
		}
		return m.reconcileEndpointSlice(ctx, fs, ip)
	}

	// Refuse to modify a Service we don't own
	if !isOwnedBy(existing.OwnerReferences, fs) {
		return fmt.Errorf("service %s/%s exists but is not owned by FailoverService %s; refusing to modify", ns, svcName, fs.Name)
	}

	// If existing is ExternalName, delete and recreate as ClusterIP
	if existing.Spec.Type == corev1.ServiceTypeExternalName {
		log.Info("Switching service type to ClusterIP", "name", svcName, "namespace", ns)
		if err := m.Client.Delete(ctx, existing); err != nil {
			return fmt.Errorf("deleting old service: %w", err)
		}
		if err := m.Client.Create(ctx, desired); err != nil {
			return err
		}
		return m.reconcileEndpointSlice(ctx, fs, ip)
	}

	// Update ports and ensure owner references
	desired.Spec.ClusterIP = existing.Spec.ClusterIP // preserve allocated ClusterIP
	existing.Spec.Ports = desired.Spec.Ports
	existing.OwnerReferences = desired.OwnerReferences
	if err := m.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating service: %w", err)
	}

	return m.reconcileEndpointSlice(ctx, fs, ip)
}

func (m *Manager) reconcileEndpointSlice(ctx context.Context, fs *failoverv1alpha1.FailoverService, ip string) error {
	sliceName := fs.Spec.ServiceName + "-failover"
	ns := fs.Namespace

	addressType := discoveryv1.AddressTypeIPv4
	if net.ParseIP(ip).To4() == nil {
		addressType = discoveryv1.AddressTypeIPv6
	}

	ports := make([]discoveryv1.EndpointPort, len(fs.Spec.Ports))
	for i, p := range fs.Spec.Ports {
		port := p.Port
		protocol := corev1.Protocol(p.Protocol)
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		name := p.Name
		ports[i] = discoveryv1.EndpointPort{
			Name:     &name,
			Port:     &port,
			Protocol: &protocol,
		}
	}

	ready := true
	desired := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sliceName,
			Namespace: ns,
			Labels: map[string]string{
				discoveryv1.LabelServiceName:             fs.Spec.ServiceName,
				"endpointslice.kubernetes.io/managed-by": "k8s-failover-manager.zyno.io",
				"app.kubernetes.io/managed-by":           "k8s-failover-manager",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(fs, failoverv1alpha1.GroupVersion.WithKind("FailoverService")),
			},
		},
		AddressType: addressType,
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{ip},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			},
		},
		Ports: ports,
	}

	existing := &discoveryv1.EndpointSlice{}
	err := m.Client.Get(ctx, types.NamespacedName{Name: sliceName, Namespace: ns}, existing)
	if errors.IsNotFound(err) {
		log.Info("Creating EndpointSlice", "name", sliceName, "namespace", ns, "ip", ip)
		return m.Client.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting endpoint slice: %w", err)
	}

	// Refuse to modify an EndpointSlice we don't own
	if !isOwnedBy(existing.OwnerReferences, fs) {
		return fmt.Errorf("endpointslice %s/%s exists but is not owned by FailoverService %s; refusing to modify", ns, sliceName, fs.Name)
	}

	// Update existing, including owner references
	existing.AddressType = addressType
	existing.Endpoints = desired.Endpoints
	existing.Ports = desired.Ports
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	return m.Client.Update(ctx, existing)
}

func (m *Manager) deleteEndpointSlice(ctx context.Context, fs *failoverv1alpha1.FailoverService) error {
	sliceName := fs.Spec.ServiceName + "-failover"
	ns := fs.Namespace

	existing := &discoveryv1.EndpointSlice{}
	err := m.Client.Get(ctx, types.NamespacedName{Name: sliceName, Namespace: ns}, existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting endpoint slice for deletion: %w", err)
	}
	if !isOwnedBy(existing.OwnerReferences, fs) {
		return fmt.Errorf("endpointslice %s/%s exists but is not owned by FailoverService %s; refusing to delete", ns, sliceName, fs.Name)
	}
	return m.Client.Delete(ctx, existing)
}

func (m *Manager) buildExternalNameService(fs *failoverv1alpha1.FailoverService, dnsName string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fs.Spec.ServiceName,
			Namespace: fs.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "k8s-failover-manager",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(fs, failoverv1alpha1.GroupVersion.WithKind("FailoverService")),
			},
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: dnsName,
		},
	}
}

// isOwnedBy checks if the given owner references include the FailoverService.
func isOwnedBy(refs []metav1.OwnerReference, fs *failoverv1alpha1.FailoverService) bool {
	for _, ref := range refs {
		if ref.UID == fs.UID {
			return true
		}
	}
	return false
}

func (m *Manager) buildClusterIPService(fs *failoverv1alpha1.FailoverService) *corev1.Service {
	ports := make([]corev1.ServicePort, len(fs.Spec.Ports))
	for i, p := range fs.Spec.Ports {
		protocol := corev1.Protocol(p.Protocol)
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		ports[i] = corev1.ServicePort{
			Name:       p.Name,
			Port:       p.Port,
			TargetPort: intstr.FromInt32(p.Port),
			Protocol:   protocol,
		}
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fs.Spec.ServiceName,
			Namespace: fs.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "k8s-failover-manager",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(fs, failoverv1alpha1.GroupVersion.WithKind("FailoverService")),
			},
		},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeClusterIP,
			Ports: ports,
		},
	}
}
