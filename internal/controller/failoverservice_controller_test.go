/*
Copyright 2026 Signal24.

Licensed under the MIT License.
See LICENSE file in the project root for full license text.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	k8sfailoverv1alpha1 "github.com/zyno-io/k8s-failover-manager/api/v1alpha1"
	"github.com/zyno-io/k8s-failover-manager/internal/action"
	"github.com/zyno-io/k8s-failover-manager/internal/connkill"
	"github.com/zyno-io/k8s-failover-manager/internal/remotecluster"
	svcmanager "github.com/zyno-io/k8s-failover-manager/internal/service"
)

// fakeSyncer is a no-op syncer for tests.
type fakeSyncer struct{}

func (f *fakeSyncer) Sync(_ context.Context, _ *k8sfailoverv1alpha1.FailoverService) (*remotecluster.SyncResult, error) {
	return &remotecluster.SyncResult{}, nil
}

func newTestReconciler() *FailoverServiceReconciler {
	waitReadyExec := &action.WaitReadyExecutor{Client: k8sClient}
	return &FailoverServiceReconciler{
		Client:      k8sClient,
		Scheme:      k8sClient.Scheme(),
		ClusterRole: "primary",
		ClusterID:   "test-cluster",
		Syncer:      &fakeSyncer{},
		ServiceManager: &svcmanager.Manager{
			Client:      k8sClient,
			ClusterRole: "primary",
		},
		ConnSignaler: &connkill.Signaler{
			Client:    k8sClient,
			Namespace: "default",
		},
		ActionExecutor: &action.MultiExecutor{
			HTTP:      &action.HTTPExecutor{},
			Job:       &action.JobExecutor{Client: k8sClient},
			Scale:     &action.ScaleExecutor{Client: k8sClient, WaitReady: waitReadyExec},
			WaitReady: waitReadyExec,
		},
	}
}

var _ = Describe("FailoverService Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-failover"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			By("creating the custom resource for the Kind FailoverService")
			failoverservice := &k8sfailoverv1alpha1.FailoverService{}
			err := k8sClient.Get(ctx, typeNamespacedName, failoverservice)
			if err != nil && errors.IsNotFound(err) {
				resource := &k8sfailoverv1alpha1.FailoverService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: k8sfailoverv1alpha1.FailoverServiceSpec{
						ServiceName: "test-service",
						Ports: []k8sfailoverv1alpha1.Port{
							{Name: "http", Port: 8080, Protocol: "TCP"},
						},
						FailoverActive: false,
						PrimaryCluster: k8sfailoverv1alpha1.ClusterTarget{
							PrimaryModeAddress:  "primary.example.com",
							FailoverModeAddress: "10.0.0.2",
						},
						FailoverCluster: k8sfailoverv1alpha1.ClusterTarget{
							PrimaryModeAddress:  "10.0.0.1",
							FailoverModeAddress: "failover.example.com",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &k8sfailoverv1alpha1.FailoverService{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				// Remove finalizer before deleting
				resource.Finalizers = nil
				_ = k8sClient.Update(ctx, resource)
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}

			// Clean up the created service
			svc := &corev1.Service{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-service", Namespace: "default"}, svc)
			if err == nil {
				_ = k8sClient.Delete(ctx, svc)
			}

			// Clean up the EndpointSlice
			eps := &discoveryv1.EndpointSlice{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-service-failover", Namespace: "default"}, eps)
			if err == nil {
				_ = k8sClient.Delete(ctx, eps)
			}
		})

		It("should successfully reconcile and create an ExternalName service", func() {
			By("Reconciling the created resource")
			controllerReconciler := newTestReconciler()

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(antiEntropyInterval))

			By("Verifying the Service was created")
			svc := &corev1.Service{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      "test-service",
				Namespace: "default",
			}, svc)
			Expect(err).NotTo(HaveOccurred())
			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeExternalName))
			Expect(svc.Spec.ExternalName).To(Equal("primary.example.com"))

			By("Verifying status was updated")
			fs := &k8sfailoverv1alpha1.FailoverService{}
			err = k8sClient.Get(ctx, typeNamespacedName, fs)
			Expect(err).NotTo(HaveOccurred())
			Expect(fs.Status.LastKnownFailoverActive).NotTo(BeNil())
			Expect(*fs.Status.LastKnownFailoverActive).To(BeFalse())
		})

		It("should detect failover transitions and start transition", func() {
			By("First reconciling to establish baseline")
			controllerReconciler := newTestReconciler()

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Toggling failover state")
			fs := &k8sfailoverv1alpha1.FailoverService{}
			err = k8sClient.Get(ctx, typeNamespacedName, fs)
			Expect(err).NotTo(HaveOccurred())
			fs.Spec.FailoverActive = true
			Expect(k8sClient.Update(ctx, fs)).To(Succeed())

			By("Reconciling after toggle — should start transition")
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(), "should use immediate requeue")

			By("Verifying transition started (no actions → goes to UpdatingResources)")
			err = k8sClient.Get(ctx, typeNamespacedName, fs)
			Expect(err).NotTo(HaveOccurred())
			Expect(fs.Status.Transition).NotTo(BeNil())
			Expect(fs.Status.Transition.Phase).To(Equal(k8sfailoverv1alpha1.PhaseUpdatingResources))
			Expect(fs.Status.Transition.TargetDirection).To(Equal("failover"))

			By("Reconciling through UpdatingResources phase")
			result, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero(), "should use immediate requeue")

			By("Reconciling through FlushingConnections phase — signal sent")
			err = k8sClient.Get(ctx, typeNamespacedName, fs)
			Expect(err).NotTo(HaveOccurred())
			Expect(fs.Status.Transition.Phase).To(Equal(k8sfailoverv1alpha1.PhaseFlushingConnections))

			result, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Reconciling through FlushingConnections phase — ACKs confirmed")
			result, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying transition completed")
			err = k8sClient.Get(ctx, typeNamespacedName, fs)
			Expect(err).NotTo(HaveOccurred())
			Expect(fs.Status.Transition).To(BeNil())
			Expect(fs.Status.LastKnownFailoverActive).NotTo(BeNil())
			Expect(*fs.Status.LastKnownFailoverActive).To(BeTrue())
			Expect(fs.Status.LastTransitionTime).NotTo(BeNil())
			Expect(fs.Status.ActiveCluster).To(Equal("test-cluster"))
			Expect(fs.Status.ActiveGeneration).To(BeNumerically(">", 0))
		})

		It("should handle transition with no actions (backward compatible)", func() {
			By("Setting up baseline")
			controllerReconciler := newTestReconciler()

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Triggering failover")
			fs := &k8sfailoverv1alpha1.FailoverService{}
			err = k8sClient.Get(ctx, typeNamespacedName, fs)
			Expect(err).NotTo(HaveOccurred())
			fs.Spec.FailoverActive = true
			Expect(k8sClient.Update(ctx, fs)).To(Succeed())

			By("Running through all phases")
			// Phase: detect transition → start
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Phase: UpdatingResources → FlushingConnections
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Phase: FlushingConnections → signal sent (0 nodes, requeues)
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Phase: FlushingConnections → ACKs confirmed (0 nodes) → complete
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying completed transition")
			err = k8sClient.Get(ctx, typeNamespacedName, fs)
			Expect(err).NotTo(HaveOccurred())
			Expect(fs.Status.Transition).To(BeNil())
			Expect(*fs.Status.LastKnownFailoverActive).To(BeTrue())

			By("Verifying leader identity was set")
			Expect(fs.Status.ActiveCluster).To(Equal("test-cluster"))
			Expect(fs.Status.ActiveGeneration).To(BeNumerically(">", 0))
		})
	})

	Context("When using pre/post actions", func() {
		const resourceName = "test-actions"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			failoverservice := &k8sfailoverv1alpha1.FailoverService{}
			err := k8sClient.Get(ctx, typeNamespacedName, failoverservice)
			if err != nil && errors.IsNotFound(err) {
				body := `{"text":"failover"}`
				resource := &k8sfailoverv1alpha1.FailoverService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: k8sfailoverv1alpha1.FailoverServiceSpec{
						ServiceName: "test-actions-svc",
						Ports: []k8sfailoverv1alpha1.Port{
							{Name: "http", Port: 8080, Protocol: "TCP"},
						},
						FailoverActive: false,
						PrimaryCluster: k8sfailoverv1alpha1.ClusterTarget{
							PrimaryModeAddress:  "primary.example.com",
							FailoverModeAddress: "10.0.0.2",
							OnFailover: &k8sfailoverv1alpha1.TransitionActions{
								PreActions: []k8sfailoverv1alpha1.FailoverAction{
									{
										Name: "notify-pre",
										HTTP: &k8sfailoverv1alpha1.HTTPAction{
											URL:    "http://localhost:9999/pre",
											Method: "POST",
											Body:   &body,
										},
										IgnoreFailure: true,
									},
								},
								PostActions: []k8sfailoverv1alpha1.FailoverAction{
									{
										Name: "notify-post",
										HTTP: &k8sfailoverv1alpha1.HTTPAction{
											URL:    "http://localhost:9999/post",
											Method: "POST",
										},
										IgnoreFailure: true,
									},
								},
							},
						},
						FailoverCluster: k8sfailoverv1alpha1.ClusterTarget{
							PrimaryModeAddress:  "10.0.0.1",
							FailoverModeAddress: "failover.example.com",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &k8sfailoverv1alpha1.FailoverService{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				resource.Finalizers = nil
				_ = k8sClient.Update(ctx, resource)
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
			svc := &corev1.Service{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-actions-svc", Namespace: "default"}, svc)
			if err == nil {
				_ = k8sClient.Delete(ctx, svc)
			}

			// Clean up the EndpointSlice
			eps := &discoveryv1.EndpointSlice{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-actions-svc-failover", Namespace: "default"}, eps)
			if err == nil {
				_ = k8sClient.Delete(ctx, eps)
			}
		})

		It("should execute pre-actions and advance through phases", func() {
			controllerReconciler := newTestReconciler()

			By("Baseline reconcile")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Triggering failover")
			fs := &k8sfailoverv1alpha1.FailoverService{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, fs)).To(Succeed())
			fs.Spec.FailoverActive = true
			Expect(k8sClient.Update(ctx, fs)).To(Succeed())

			By("Start transition → should go to ExecutingPreActions")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, fs)).To(Succeed())
			Expect(fs.Status.Transition).NotTo(BeNil())
			Expect(fs.Status.Transition.Phase).To(Equal(k8sfailoverv1alpha1.PhaseExecutingPreActions))

			By("Executing pre-action (HTTP will fail but ignoreFailure=true)")
			// This first reconcile marks the action Running. Subsequent reconciles execute/retry.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Transport failures are retried; allow enough loops for retry exhaustion
			// and subsequent ignoreFailure skip/phase advance.
			for range 12 {
				Expect(k8sClient.Get(ctx, typeNamespacedName, fs)).To(Succeed())
				if fs.Status.Transition == nil || fs.Status.Transition.Phase != k8sfailoverv1alpha1.PhaseExecutingPreActions {
					break
				}
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())
			}

			By("Should have advanced past pre-actions")
			Expect(k8sClient.Get(ctx, typeNamespacedName, fs)).To(Succeed())
			Expect(fs.Status.Transition).NotTo(BeNil())
			// Should be in UpdatingResources, FlushingConnections, or ExecutingPostActions
			Expect(fs.Status.Transition.Phase).NotTo(Equal(k8sfailoverv1alpha1.PhaseExecutingPreActions))
		})

		It("should handle force-advance", func() {
			controllerReconciler := newTestReconciler()

			By("Baseline and trigger failover")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			fs := &k8sfailoverv1alpha1.FailoverService{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, fs)).To(Succeed())
			fs.Spec.FailoverActive = true
			Expect(k8sClient.Update(ctx, fs)).To(Succeed())

			// Start transition
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Adding force-advance annotation")
			Expect(k8sClient.Get(ctx, typeNamespacedName, fs)).To(Succeed())
			if fs.Annotations == nil {
				fs.Annotations = make(map[string]string)
			}
			fs.Annotations[forceAdvanceAnnotation] = "true"
			Expect(k8sClient.Update(ctx, fs)).To(Succeed())

			By("Reconciling with force-advance")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying force-advance cleared annotation and advanced phase")
			Expect(k8sClient.Get(ctx, typeNamespacedName, fs)).To(Succeed())
			Expect(fs.Annotations[forceAdvanceAnnotation]).To(BeEmpty())
		})
	})
})
