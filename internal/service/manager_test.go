package service

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	failoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = failoverv1alpha1.AddToScheme(s)
	return s
}

func newFailoverService(svcName string, failoverActive bool) *failoverv1alpha1.FailoverService {
	return &failoverv1alpha1.FailoverService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
			UID:       "test-uid",
		},
		Spec: failoverv1alpha1.FailoverServiceSpec{
			ServiceName:    svcName,
			FailoverActive: failoverActive,
			Ports: []failoverv1alpha1.Port{
				{Name: "mysql", Port: 3306, Protocol: "TCP"},
			},
			PrimaryCluster: failoverv1alpha1.ClusterTarget{
				PrimaryModeAddress:  "mysql-primary.example.com",
				FailoverModeAddress: "192.168.50.60",
			},
			FailoverCluster: failoverv1alpha1.ClusterTarget{
				PrimaryModeAddress:  "10.20.30.40",
				FailoverModeAddress: "mysql-failover.example.com",
			},
		},
	}
}

func TestResolveActiveAddress(t *testing.T) {
	fs := newFailoverService("mysql", false)

	tests := []struct {
		name           string
		clusterRole    string
		failoverActive bool
		expected       string
	}{
		{
			name:           "primary cluster, primary active",
			clusterRole:    "primary",
			failoverActive: false,
			expected:       "mysql-primary.example.com", // primaryCluster.primaryModeAddress
		},
		{
			name:           "failover cluster, primary active",
			clusterRole:    "failover",
			failoverActive: false,
			expected:       "10.20.30.40", // failoverCluster.primaryModeAddress
		},
		{
			name:           "primary cluster, failover active",
			clusterRole:    "primary",
			failoverActive: true,
			expected:       "192.168.50.60", // primaryCluster.failoverModeAddress
		},
		{
			name:           "failover cluster, failover active",
			clusterRole:    "failover",
			failoverActive: true,
			expected:       "mysql-failover.example.com", // failoverCluster.failoverModeAddress
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &Manager{ClusterRole: tt.clusterRole}
			fs.Spec.FailoverActive = tt.failoverActive
			got := mgr.ResolveActiveAddress(fs)
			if got != tt.expected {
				t.Errorf("ResolveActiveAddress() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestReconcileExternalNameService(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", false)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	// First reconcile should create ExternalName service
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}

	if svc.Spec.Type != corev1.ServiceTypeExternalName {
		t.Errorf("Expected ExternalName service, got %s", svc.Spec.Type)
	}
	if svc.Spec.ExternalName != "mysql-primary.example.com" {
		t.Errorf("Expected ExternalName mysql-primary.example.com, got %s", svc.Spec.ExternalName)
	}
}

func TestReconcileClusterIPService(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	// failover active + primary cluster → use primaryCluster.failoverModeAddress (IP: 192.168.50.60)
	fs := newFailoverService("mysql-svc", true)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}

	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Expected ClusterIP service, got %s", svc.Spec.Type)
	}

	// Check EndpointSlice was created
	eps := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc-failover", Namespace: "default"}, eps); err != nil {
		t.Fatalf("Get endpoint slice error: %v", err)
	}

	if len(eps.Endpoints) != 1 || eps.Endpoints[0].Addresses[0] != "192.168.50.60" {
		t.Errorf("Expected endpoint 192.168.50.60, got %v", eps.Endpoints)
	}
}

func TestServiceTypeTransition(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", false)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	// Create ExternalName service first
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("First reconcile error: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeExternalName {
		t.Fatalf("Expected ExternalName, got %s", svc.Spec.Type)
	}

	// Toggle failover → ClusterIP
	fs.Spec.FailoverActive = true
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Second reconcile error: %v", err)
	}

	svc = &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service after transition error: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Expected ClusterIP after transition, got %s", svc.Spec.Type)
	}

	// Toggle back → ExternalName
	fs.Spec.FailoverActive = false
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Third reconcile error: %v", err)
	}

	svc = &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service after second transition error: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeExternalName {
		t.Errorf("Expected ExternalName after second transition, got %s", svc.Spec.Type)
	}
}

func TestReconcile_ExternalNameToClusterIP(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", false)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	// Create ExternalName service (primary mode, DNS address)
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("First reconcile error: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeExternalName {
		t.Fatalf("expected ExternalName, got %s", svc.Spec.Type)
	}

	// Switch to failover → now address is IP 192.168.50.60 → ClusterIP
	fs.Spec.FailoverActive = true
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Second reconcile error: %v", err)
	}

	svc = &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("expected ClusterIP after ExternalName→ClusterIP transition, got %s", svc.Spec.Type)
	}
}

func TestReconcile_ClusterIPToExternalName(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	// Start with failover active (IP address → ClusterIP)
	fs := newFailoverService("mysql-svc", true)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	// Create ClusterIP service
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("First reconcile error: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("expected ClusterIP, got %s", svc.Spec.Type)
	}

	// Switch back to primary → DNS address → ExternalName
	fs.Spec.FailoverActive = false
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Second reconcile error: %v", err)
	}

	svc = &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeExternalName {
		t.Errorf("expected ExternalName after ClusterIP→ExternalName transition, got %s", svc.Spec.Type)
	}
}

func TestReconcile_ServiceNotOwned(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", false)

	// Create a service owned by someone else (different UID)
	existingSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mysql-svc",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "other-owner",
					UID:        "different-uid",
				},
			},
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "other.example.com",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingSvc).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	err := mgr.Reconcile(ctx, fs)
	if err == nil {
		t.Fatal("expected error when service has wrong OwnerReference")
	}
}

func TestReconcile_ClusterIPServiceNotOwned(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	// Use failover active to get IP address (ClusterIP path)
	fs := newFailoverService("mysql-svc", true)

	existingSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mysql-svc",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other-uid"},
			},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingSvc).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	err := mgr.Reconcile(ctx, fs)
	if err == nil {
		t.Fatal("expected error when ClusterIP service has wrong OwnerReference")
	}
}

func TestReconcile_ExternalNameSameDNS_NoUpdate(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", false)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	// Create the service
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("First reconcile error: %v", err)
	}

	// Reconcile again with the same DNS name — should be a no-op
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Second reconcile error: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}
	if svc.Spec.ExternalName != "mysql-primary.example.com" {
		t.Errorf("expected ExternalName unchanged, got %s", svc.Spec.ExternalName)
	}
}

func TestReconcile_ExternalNameDNSChange(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", false)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	// Create the service
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("First reconcile error: %v", err)
	}

	// Change the DNS address and reconcile
	fs.Spec.PrimaryCluster.PrimaryModeAddress = "new-primary.example.com"
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Second reconcile error: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}
	if svc.Spec.ExternalName != "new-primary.example.com" {
		t.Errorf("expected updated ExternalName, got %s", svc.Spec.ExternalName)
	}
}

func TestReconcile_ClusterIPServiceUpdate(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", true)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	// Create ClusterIP service
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("First reconcile error: %v", err)
	}

	// Update IP address and reconcile again
	fs.Spec.PrimaryCluster.FailoverModeAddress = "192.168.50.99"
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Second reconcile error: %v", err)
	}

	// Verify EndpointSlice updated
	eps := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc-failover", Namespace: "default"}, eps); err != nil {
		t.Fatalf("Get endpoint slice error: %v", err)
	}
	if len(eps.Endpoints) != 1 || eps.Endpoints[0].Addresses[0] != "192.168.50.99" {
		t.Errorf("expected updated endpoint 192.168.50.99, got %v", eps.Endpoints)
	}
}

func TestReconcile_EndpointSliceNotOwned(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", true)

	// Create a properly owned ClusterIP service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mysql-svc",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "k8s-failover.zyno.io/v1alpha1", Kind: "FailoverService", Name: "test", UID: "test-uid"},
			},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
	}

	// Create an EndpointSlice owned by someone else
	eps := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mysql-svc-failover",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other-uid"},
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, eps).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	err := mgr.Reconcile(ctx, fs)
	if err == nil {
		t.Fatal("expected error when EndpointSlice has wrong OwnerReference")
	}
}

func TestReconcile_ProtocolDefault(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", true)
	// Clear protocol to test default
	fs.Spec.Ports[0].Protocol = ""

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("expected default protocol TCP, got %s", svc.Spec.Ports[0].Protocol)
	}

	eps := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc-failover", Namespace: "default"}, eps); err != nil {
		t.Fatalf("Get endpoint slice error: %v", err)
	}
	if len(eps.Ports) != 1 || *eps.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("expected default protocol TCP in EndpointSlice, got %v", eps.Ports)
	}
}

func TestReconcile_IPv6Address(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", true)
	fs.Spec.PrimaryCluster.FailoverModeAddress = "fd00::1"

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	eps := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc-failover", Namespace: "default"}, eps); err != nil {
		t.Fatalf("Get endpoint slice error: %v", err)
	}
	if eps.AddressType != discoveryv1.AddressTypeIPv6 {
		t.Errorf("expected IPv6 address type, got %s", eps.AddressType)
	}
	if len(eps.Endpoints) != 1 || eps.Endpoints[0].Addresses[0] != "fd00::1" {
		t.Errorf("expected endpoint fd00::1, got %v", eps.Endpoints)
	}
}

func TestReconcile_FailoverClusterRole(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", false)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "failover"}

	// failover role + primary mode → failoverCluster.primaryModeAddress = 10.20.30.40 (IP)
	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("expected ClusterIP for IP address, got %s", svc.Spec.Type)
	}

	eps := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc-failover", Namespace: "default"}, eps); err != nil {
		t.Fatalf("Get endpoint slice error: %v", err)
	}
	if len(eps.Endpoints) != 1 || eps.Endpoints[0].Addresses[0] != "10.20.30.40" {
		t.Errorf("expected endpoint 10.20.30.40, got %v", eps.Endpoints)
	}
}

func TestReconcile_MultiplePorts(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", true)
	fs.Spec.Ports = []failoverv1alpha1.Port{
		{Name: "mysql", Port: 3306, Protocol: "TCP"},
		{Name: "metrics", Port: 9090, Protocol: "TCP"},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	if err := mgr.Reconcile(ctx, fs); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}
	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("expected 2 ports, got %d", len(svc.Spec.Ports))
	}
	if svc.Spec.Ports[0].Port != 3306 || svc.Spec.Ports[1].Port != 9090 {
		t.Errorf("unexpected ports: %v", svc.Spec.Ports)
	}

	eps := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc-failover", Namespace: "default"}, eps); err != nil {
		t.Fatalf("Get endpoint slice error: %v", err)
	}
	if len(eps.Ports) != 2 {
		t.Fatalf("expected 2 EndpointSlice ports, got %d", len(eps.Ports))
	}
}

func TestReconcileWithDirection(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", false)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	// Use ReconcileWithDirection to force failover mode even though spec says primary
	if err := mgr.ReconcileWithDirection(ctx, fs, true); err != nil {
		t.Fatalf("ReconcileWithDirection error: %v", err)
	}

	// Should use primaryCluster.failoverModeAddress = 192.168.50.60 (IP → ClusterIP)
	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: "mysql-svc", Namespace: "default"}, svc); err != nil {
		t.Fatalf("Get service error: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("expected ClusterIP, got %s", svc.Spec.Type)
	}
}

func TestDeleteEndpointSlice_NotOwned(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", false)

	// Create an EndpointSlice owned by someone else
	eps := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mysql-svc-failover",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other-uid"},
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(eps).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	err := mgr.deleteEndpointSlice(ctx, fs)
	if err == nil {
		t.Fatal("expected error when EndpointSlice has wrong OwnerReference")
	}
}

func TestDeleteEndpointSlice_NotFound(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", false)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	err := mgr.deleteEndpointSlice(ctx, fs)
	if err != nil {
		t.Fatalf("expected no error for missing EndpointSlice, got: %v", err)
	}
}

func TestDeleteEndpointSlice_OwnedSuccessfullyDeleted(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme()
	fs := newFailoverService("mysql-svc", false)

	eps := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mysql-svc-failover",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "k8s-failover.zyno.io/v1alpha1", Kind: "FailoverService", Name: "test", UID: "test-uid"},
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(eps).Build()
	mgr := &Manager{Client: c, ClusterRole: "primary"}

	err := mgr.deleteEndpointSlice(ctx, fs)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify it was actually deleted
	got := &discoveryv1.EndpointSlice{}
	err = c.Get(ctx, types.NamespacedName{Name: "mysql-svc-failover", Namespace: "default"}, got)
	if err == nil {
		t.Fatal("expected EndpointSlice to be deleted")
	}
}

func TestIsOwnedBy(t *testing.T) {
	fs := newFailoverService("mysql-svc", false)

	tests := []struct {
		name     string
		refs     []metav1.OwnerReference
		expected bool
	}{
		{
			name:     "no owner references",
			refs:     nil,
			expected: false,
		},
		{
			name: "matching UID",
			refs: []metav1.OwnerReference{
				{UID: fs.UID},
			},
			expected: true,
		},
		{
			name: "non-matching UID",
			refs: []metav1.OwnerReference{
				{UID: "different-uid"},
			},
			expected: false,
		},
		{
			name: "matching UID among multiple refs",
			refs: []metav1.OwnerReference{
				{UID: "other-uid-1"},
				{UID: fs.UID},
				{UID: "other-uid-2"},
			},
			expected: true,
		},
		{
			name:     "empty refs slice",
			refs:     []metav1.OwnerReference{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isOwnedBy(tt.refs, fs)
			if got != tt.expected {
				t.Errorf("isOwnedBy() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestBuildExternalNameService(t *testing.T) {
	fs := newFailoverService("mysql-svc", false)
	mgr := &Manager{ClusterRole: "primary"}

	svc := mgr.buildExternalNameService(fs, "db.example.com")

	if svc.Name != "mysql-svc" {
		t.Errorf("expected name mysql-svc, got %s", svc.Name)
	}
	if svc.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", svc.Namespace)
	}
	if svc.Spec.Type != corev1.ServiceTypeExternalName {
		t.Errorf("expected ExternalName type, got %s", svc.Spec.Type)
	}
	if svc.Spec.ExternalName != "db.example.com" {
		t.Errorf("expected ExternalName db.example.com, got %s", svc.Spec.ExternalName)
	}
	if len(svc.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(svc.OwnerReferences))
	}
	if svc.OwnerReferences[0].UID != fs.UID {
		t.Errorf("expected owner UID %s, got %s", fs.UID, svc.OwnerReferences[0].UID)
	}
	if svc.Labels["app.kubernetes.io/managed-by"] != "k8s-failover-manager" {
		t.Errorf("expected managed-by label, got %v", svc.Labels)
	}
}

func TestBuildClusterIPService(t *testing.T) {
	fs := newFailoverService("mysql-svc", true)
	mgr := &Manager{ClusterRole: "primary"}

	svc := mgr.buildClusterIPService(fs)

	if svc.Name != "mysql-svc" {
		t.Errorf("expected name mysql-svc, got %s", svc.Name)
	}
	if svc.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", svc.Namespace)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("expected ClusterIP type, got %s", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(svc.Spec.Ports))
	}
	if svc.Spec.Ports[0].Port != 3306 {
		t.Errorf("expected port 3306, got %d", svc.Spec.Ports[0].Port)
	}
	if svc.Spec.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("expected TCP protocol, got %s", svc.Spec.Ports[0].Protocol)
	}
	if svc.Spec.Ports[0].Name != "mysql" {
		t.Errorf("expected port name mysql, got %s", svc.Spec.Ports[0].Name)
	}
	if len(svc.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(svc.OwnerReferences))
	}
	if svc.Labels["app.kubernetes.io/managed-by"] != "k8s-failover-manager" {
		t.Errorf("expected managed-by label, got %v", svc.Labels)
	}
}

func TestBuildClusterIPService_DefaultProtocol(t *testing.T) {
	fs := newFailoverService("mysql-svc", true)
	fs.Spec.Ports[0].Protocol = "" // empty should default to TCP
	mgr := &Manager{ClusterRole: "primary"}

	svc := mgr.buildClusterIPService(fs)

	if svc.Spec.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("expected default TCP protocol, got %s", svc.Spec.Ports[0].Protocol)
	}
}

func TestBuildClusterIPService_MultiplePorts(t *testing.T) {
	fs := newFailoverService("mysql-svc", true)
	fs.Spec.Ports = []failoverv1alpha1.Port{
		{Name: "mysql", Port: 3306, Protocol: "TCP"},
		{Name: "metrics", Port: 9090, Protocol: "TCP"},
		{Name: "admin", Port: 8080, Protocol: "UDP"},
	}
	mgr := &Manager{ClusterRole: "primary"}

	svc := mgr.buildClusterIPService(fs)

	if len(svc.Spec.Ports) != 3 {
		t.Fatalf("expected 3 ports, got %d", len(svc.Spec.Ports))
	}
	if svc.Spec.Ports[2].Protocol != corev1.ProtocolUDP {
		t.Errorf("expected UDP protocol for admin port, got %s", svc.Spec.Ports[2].Protocol)
	}
}

func TestResolveActiveAddressWithDirection(t *testing.T) {
	fs := newFailoverService("mysql-svc", false)

	tests := []struct {
		name           string
		clusterRole    string
		failoverActive bool
		expected       string
	}{
		{"primary+primary", "primary", false, "mysql-primary.example.com"},
		{"primary+failover", "primary", true, "192.168.50.60"},
		{"failover+primary", "failover", false, "10.20.30.40"},
		{"failover+failover", "failover", true, "mysql-failover.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &Manager{ClusterRole: tt.clusterRole}
			got := mgr.ResolveActiveAddressWithDirection(fs, tt.failoverActive)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}
