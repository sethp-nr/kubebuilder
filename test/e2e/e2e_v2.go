/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/kubebuilder/test/e2e/scaffold"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("kubebuilder", func() {
	Context("with v2 scaffolding", func() {
		var kbc *KBTestContext
		BeforeEach(func() {
			var err error
			kbc, err = TestContext("GO111MODULE=on")
			Expect(err).NotTo(HaveOccurred())
			Expect(kbc.Prepare()).To(Succeed())

			By("installing cert manager bundle")
			Expect(kbc.InstallCertManager()).To(Succeed())
		})

		AfterEach(func() {
			By("clean up created API objects during test process")
			kbc.CleanupManifests(filepath.Join("config", "default"))

			By("uninstalling cert manager bundle")
			kbc.UninstallCertManager()

			By("remove container image and work dir")
			kbc.Destroy()
		})

		It("should generate a runnable project", func() {
			var controllerPodName string
			By("init v2 project")
			err := kbc.Init(
				"--project-version", "2",
				"--domain", kbc.Domain,
				"--dep=false")
			Expect(err).Should(Succeed())

			By("creating api definition")
			err = kbc.CreateAPI(
				"--group", kbc.Group,
				"--version", kbc.Version,
				"--kind", kbc.Kind,
				"--namespaced",
				"--resource",
				"--controller",
				"--make=false")
			Expect(err).Should(Succeed())

			By("implementing the API")
			Expect(insertCode(
				filepath.Join(kbc.Dir, "api", kbc.Version, fmt.Sprintf("%s_types.go", strings.ToLower(kbc.Kind))),
				fmt.Sprintf(`type %sSpec struct {
`, kbc.Kind),
				`	// +optional
	Count int `+"`"+`json:"count,omitempty"`+"`"+`
`)).Should(Succeed())

			By("implementing the mutating and validating webhooks")
			err = (&scaffold.Webhook{
				Domain:    kbc.Domain,
				Group:     kbc.Group,
				Version:   kbc.Version,
				Kind:      kbc.Kind,
				Resources: kbc.Resources,
			}).WriteTo(filepath.Join(
				kbc.Dir, "api", kbc.Version,
				fmt.Sprintf("%s_webhook.go", strings.ToLower(kbc.Kind))))
			Expect(err).Should(Succeed())

			By("uncomment kustomization.yaml to enable webhook and ca injection")
			Expect(uncommentCode(
				filepath.Join(kbc.Dir, "config", "default", "kustomization.yaml"),
				"#- ../webhook", "#")).To(Succeed())
			Expect(uncommentCode(
				filepath.Join(kbc.Dir, "config", "default", "kustomization.yaml"),
				"#- ../certmanager", "#")).To(Succeed())
			Expect(uncommentCode(
				filepath.Join(kbc.Dir, "config", "default", "kustomization.yaml"),
				"#- manager_webhook_patch.yaml", "#")).To(Succeed())
			Expect(uncommentCode(
				filepath.Join(kbc.Dir, "config", "default", "kustomization.yaml"),
				"#- webhookcainjection_patch.yaml", "#")).To(Succeed())

			By("building image")
			err = kbc.Make("docker-build", "IMG="+kbc.ImageName)
			Expect(err).Should(Succeed())

			By("loading docker image into kind cluster")
			err = kbc.LoadImageToKindCluster()
			Expect(err).Should(Succeed())

			// NOTE: If you want to run the test against a GKE cluster, you will need to grant yourself permission.
			// Otherwise, you may see "... is forbidden: attempt to grant extra privileges"
			// $ kubectl create clusterrolebinding myname-cluster-admin-binding --clusterrole=cluster-admin --user=myname@mycompany.com
			// https://cloud.google.com/kubernetes-engine/docs/how-to/role-based-access-control
			By("deploying controller manager")
			err = kbc.Make("deploy")
			Expect(err).Should(Succeed())

			By("validate the controller-manager pod running as expected")
			verifyControllerUp := func() error {
				// Get pod name
				podOutput, err := kbc.Kubectl.Get(
					true,
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}{{ end }}")
				Expect(err).NotTo(HaveOccurred())
				podNames := getNonEmptyLines(podOutput)
				if len(podNames) != 1 {
					return fmt.Errorf("expect 1 controller pods running, but got %d", len(podNames))
				}
				controllerPodName = podNames[0]
				Expect(controllerPodName).Should(ContainSubstring("controller-manager"))

				// Validate pod status
				status, err := kbc.Kubectl.Get(
					true,
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}")
				Expect(err).NotTo(HaveOccurred())
				if status != "Running" {
					return fmt.Errorf("controller pod in %s status", status)
				}
				return nil
			}
			Eventually(verifyControllerUp, time.Minute, time.Second).Should(Succeed())

			By("validate cert manager has provisioned the certificate secret")
			Eventually(func() error {
				_, err := kbc.Kubectl.Get(
					true,
					"secrets", "webhook-server-cert")
				return err
			}, time.Minute, time.Second).Should(Succeed())

			By("validate the mutating|validating webhooks have the CA injected")
			verifyCAInjection := func() error {
				mwhOutput, err := kbc.Kubectl.Get(
					false,
					"mutatingwebhookconfigurations.admissionregistration.k8s.io",
					fmt.Sprintf("e2e-%s-mutating-webhook-configuration", kbc.TestSuffix),
					"-o", "go-template={{ range .webhooks }}{{ .clientConfig.caBundle }}{{ end }}")
				Expect(err).NotTo(HaveOccurred())
				// sanity check that ca should be long enough, because there may be a place holder "\n"
				Expect(len(mwhOutput)).To(BeNumerically(">", 10))

				vwhOutput, err := kbc.Kubectl.Get(
					false,
					"validatingwebhookconfigurations.admissionregistration.k8s.io",
					fmt.Sprintf("e2e-%s-validating-webhook-configuration", kbc.TestSuffix),
					"-o", "go-template={{ range .webhooks }}{{ .clientConfig.caBundle }}{{ end }}")
				Expect(err).NotTo(HaveOccurred())
				// sanity check that ca should be long enough, because there may be a place holder "\n"
				Expect(len(vwhOutput)).To(BeNumerically(">", 10))

				return nil
			}
			Eventually(verifyCAInjection, time.Minute, time.Second).Should(Succeed())

			By("creating an instance of CR")
			// currently controller-runtime doesn't provide a readiness probe, we retry a few times
			// we can change it to probe the readiness endpoint after CR supports it.
			sampleFile := filepath.Join("config", "samples", fmt.Sprintf("%s_%s_%s.yaml", kbc.Group, kbc.Version, strings.ToLower(kbc.Kind)))
			Eventually(func() error {
				_, err = kbc.Kubectl.Apply(true, "-f", sampleFile)
				return err
			}, time.Minute, time.Second).Should(Succeed())

			By("validate the created resource object gets reconciled in controller")
			managerContainerLogs := func() string {
				logOutput, err := kbc.Kubectl.Logs(controllerPodName, "-c", "manager")
				Expect(err).NotTo(HaveOccurred())
				return logOutput
			}
			Eventually(managerContainerLogs, time.Minute, time.Second).Should(ContainSubstring("Successfully Reconciled"))

			By("validate mutating and validating webhooks are working fine")
			cnt, err := kbc.Kubectl.Get(
				true,
				"-f", sampleFile,
				"-o", "go-template={{ .spec.count }}")
			Expect(err).NotTo(HaveOccurred())
			count, err := strconv.Atoi(cnt)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(BeNumerically("==", 5))
		})
	})
})
