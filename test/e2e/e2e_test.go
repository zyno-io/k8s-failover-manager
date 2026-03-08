//go:build e2e
// +build e2e

/*
Copyright 2026 Signal24.

Licensed under the MIT License.
See LICENSE file in the project root for full license text.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/zyno-io/k8s-failover-manager/test/utils"
)

// namespace where the project is deployed in
const namespace = "k8s-failover-manager-system"

// serviceAccountName created for the project
const serviceAccountName = "k8s-failover-manager-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "k8s-failover-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "k8s-failover-manager-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command(
			"make",
			"deploy",
			fmt.Sprintf("IMG=%s", managerImage),
			fmt.Sprintf("DEPLOY_NAMESPACE=%s", namespace),
			"CONNECTION_KILLER_ENABLED=false",
			"HELM_EXTRA_ARGS=--set failoverConfig.clusterId=e2e-cluster --set failoverConfig.remoteKubeconfigSecret=e2e-secret --set failoverConfig.remoteKubeconfigNamespace="+namespace,
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("cleaning up the metrics ClusterRoleBinding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy", fmt.Sprintf("DEPLOY_NAMESPACE=%s", namespace))
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

			It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=k8s-failover-manager-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
				Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
			})

			// +kubebuilder:scaffold:e2e-webhooks-checks

			It("should reconcile a FailoverService sample and create the managed Service", func() {
				samplePath, err := resolveSamplePath()
				Expect(err).NotTo(HaveOccurred(), "Failed to resolve sample FailoverService path")
				By("applying the FailoverService sample")
				cmd := exec.Command("kubectl", "apply", "-f", samplePath)
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to apply sample FailoverService")

				defer func() {
					// Remove finalizer first to prevent hanging on delete
					patchCmd := exec.Command("kubectl", "patch", "failoverservice", "mysql-failover",
						"-n", "default", "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
					_, _ = utils.Run(patchCmd)
					cleanupCmd := exec.Command("kubectl", "delete", "-f", samplePath, "--ignore-not-found=true", "--timeout=30s")
					_, _ = utils.Run(cleanupCmd)
					cleanupSvcCmd := exec.Command("kubectl", "delete", "service", "mysql-dns-alias", "-n", "default", "--ignore-not-found=true")
					_, _ = utils.Run(cleanupSvcCmd)
				}()

				By("verifying the managed Service becomes an ExternalName Service")
				Eventually(func(g Gomega) {
					typeCmd := exec.Command("kubectl", "get", "service", "mysql-dns-alias", "-n", "default", "-o", "jsonpath={.spec.type}")
					svcType, err := utils.Run(typeCmd)
					if err != nil {
						logCmd := exec.Command("kubectl", "logs", "-l", "control-plane=controller-manager",
							"-n", namespace, "--tail=30")
						logs, _ := utils.Run(logCmd)
						_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n%s\n", logs)

						statusCmd := exec.Command("kubectl", "get", "failoverservice", "mysql-failover",
							"-n", "default", "-o", "yaml")
						status, _ := utils.Run(statusCmd)
						_, _ = fmt.Fprintf(GinkgoWriter, "FailoverService:\n%s\n", status)

						eventsCmd := exec.Command("kubectl", "get", "events", "-n", "default",
							"--field-selector", "involvedObject.name=mysql-failover", "--sort-by=.lastTimestamp")
						events, _ := utils.Run(eventsCmd)
						_, _ = fmt.Fprintf(GinkgoWriter, "Events:\n%s\n", events)
					}
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(svcType).To(Equal("ExternalName"))

					nameCmd := exec.Command("kubectl", "get", "service", "mysql-dns-alias", "-n", "default", "-o", "jsonpath={.spec.externalName}")
					externalName, err := utils.Run(nameCmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(externalName).To(Equal("mysql-primary.example.com"))
				}, 2*time.Minute, time.Second).Should(Succeed())
			})

			It("should toggle failover and failback correctly", func() {
				const crName = "failover-toggle-test"
				const svcName = "failover-toggle-svc"
				const primaryAddr = "primary.toggle.example.com"
				const failoverAddr = "failover.toggle.example.com"

				cr := strings.NewReader(fmt.Sprintf(`apiVersion: k8s-failover.zyno.io/v1alpha1
kind: FailoverService
metadata:
  name: %s
  namespace: default
spec:
  serviceName: %s
  failoverActive: false
  ports:
    - name: tcp
      port: 3306
      protocol: TCP
  primaryCluster:
    primaryModeAddress: %s
    failoverModeAddress: %s
  failoverCluster:
    primaryModeAddress: %s
    failoverModeAddress: %s
`, crName, svcName, primaryAddr, failoverAddr, primaryAddr, failoverAddr))

				defer func() {
					patchCmd := exec.Command("kubectl", "patch", "failoverservice", crName,
						"-n", "default", "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
					_, _ = utils.Run(patchCmd)
					cleanupCmd := exec.Command("kubectl", "delete", "failoverservice", crName, "-n", "default", "--ignore-not-found=true", "--timeout=30s")
					_, _ = utils.Run(cleanupCmd)
					cleanupSvcCmd := exec.Command("kubectl", "delete", "service", svcName, "-n", "default", "--ignore-not-found=true")
					_, _ = utils.Run(cleanupSvcCmd)
				}()

				By("creating the FailoverService CR")
				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = cr
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to create FailoverService")

				By("verifying initial Service externalName is the primary address")
				Eventually(func(g Gomega) {
					nameCmd := exec.Command("kubectl", "get", "service", svcName, "-n", "default", "-o", "jsonpath={.spec.externalName}")
					externalName, err := utils.Run(nameCmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(externalName).To(Equal(primaryAddr))
				}, 2*time.Minute, time.Second).Should(Succeed())

				By("patching failoverActive to true")
				cmd = exec.Command("kubectl", "patch", "failoverservice", crName, "-n", "default",
					"--type=merge", "-p", `{"spec":{"failoverActive":true}}`)
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to patch failoverActive to true")

				By("verifying Service externalName switches to the failover address")
				Eventually(func(g Gomega) {
					nameCmd := exec.Command("kubectl", "get", "service", svcName, "-n", "default", "-o", "jsonpath={.spec.externalName}")
					externalName, err := utils.Run(nameCmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(externalName).To(Equal(failoverAddr))
				}, 2*time.Minute, time.Second).Should(Succeed())

				By("verifying transition has completed (status.transition is empty)")
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", "failoverservice", crName, "-n", "default",
						"-o", "jsonpath={.status.transition}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(BeEmpty(), "expected status.transition to be cleared after transition completes")
				}, 2*time.Minute, time.Second).Should(Succeed())

				By("patching failoverActive back to false (failback)")
				cmd = exec.Command("kubectl", "patch", "failoverservice", crName, "-n", "default",
					"--type=merge", "-p", `{"spec":{"failoverActive":false}}`)
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to patch failoverActive to false")

				By("verifying Service externalName returns to the primary address")
				Eventually(func(g Gomega) {
					nameCmd := exec.Command("kubectl", "get", "service", svcName, "-n", "default", "-o", "jsonpath={.spec.externalName}")
					externalName, err := utils.Run(nameCmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(externalName).To(Equal(primaryAddr))
				}, 2*time.Minute, time.Second).Should(Succeed())
			})
		})
	})

func resolveSamplePath() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("could not determine e2e test source location")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	samplePath := filepath.Join(repoRoot, "config", "samples", "k8s-failover_v1alpha1_failoverservice.yaml")
	if _, err := os.Stat(samplePath); err != nil {
		return "", fmt.Errorf("sample manifest not found at %q: %w", samplePath, err)
	}
	return samplePath, nil
}

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
