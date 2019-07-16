/*
Copyright 2019 The Knative Authors

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
package common

import (
	"bytes"
	"flag"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/rogpeppe/go-internal/semver"
	v1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"knative.dev/pkg/version"
	opversion "knative.dev/serving-operator/version"
)

const (
	// IstioDependency name
	istioDependency     = "istio"
	istioNamespace      = "istio-system"
	istioPilotPodLabel  = "istio=pilot"
	istioPilotContainer = "istio-proxy"
	// CertManagerDependency name
	certManagerDependency = "certmanager"
	certManagerNamespace  = "cert-manager"
)

var (
	MasterURL  = flag.String("master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	Kubeconfig = flag.String("kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
)

type depMeta struct {
	// dependent
	name string
	// in which dependent installed
	namespace string
	// deployments installed in the namespace
	deploy []string
}

var depMetas = map[string]depMeta{
	istioDependency: {
		name:      istioDependency,
		namespace: istioNamespace,
		deploy: []string{
			"istio-ingressgateway",
			"cluster-local-gateway",
		},
	},
	certManagerDependency: {
		name:      certManagerDependency,
		namespace: certManagerNamespace,
		deploy: []string{
			"cert-manager",
			"cert-manager-webhook",
		},
	},
}

func checkDependencyVersion(depname, installedVersion, expectedVersion string, log *zap.SugaredLogger) error {
	log.Infof("Installed istio version %s", installedVersion)
	ver := semver.Canonical(installedVersion)
	log.Infof("Installed istio version %s", ver)
	if semver.Compare(expectedVersion, installedVersion) == 1 {
		return fmt.Errorf("%q version %q is not compatible, need at least %q",
			depname, ver, expectedVersion)
	}
	log.Infof("%s installed version %s matched with expected version %s")
	return nil
}

func checkDeploymentVersion(deploy *v1beta1.Deployment, depName string, log *zap.SugaredLogger) error {
	var ver, expect string
	if depName == certManagerDependency {
		// for cert-manager, get version information from image tag
		for _, con := range deploy.Spec.Template.Spec.Containers {
			if con.Name == deploy.Name {
				img := strings.Split(con.Image, ":")
				if len(img) != 2 {
					log.Errorf("version check", "failed to get version from "+con.Image)
					return fmt.Errorf("failed to get %q version from %q", depName, con.Image)
				}
				ver = img[1]
			}
		}
		expect = opversion.CertManager
	}

	return checkDependencyVersion(depName, ver, expect, log)
}

// PodExec takes command and run the command in the specified container of Pod
func podExec(client kubernetes.Interface, podNamespace, podLabel,
	container string, command []string, log *zap.SugaredLogger) (*bytes.Buffer, *bytes.Buffer, error) {
	pods, err := client.CoreV1().Pods(podNamespace).List(metav1.ListOptions{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		LabelSelector: podLabel,
	})
	if err != nil || len(pods.Items) < 1 {
		log.Errorf("Dependency Validation", "failed to get pods with label", podLabel)
		return nil, nil, err
	}

	log.Infof("get istio version from pod %s", pods.Items[0].Name)
	req := client.CoreV1().RESTClient().Post().Resource("pods").Name(pods.Items[0].Name).Namespace(podNamespace).
		SubResource("exec").
		VersionedParams(&v1.PodExecOptions{
			Command:   command,
			Container: container,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, k8sscheme.ParameterCodec)

	// build kubeconfig here
	cfg, err := clientcmd.BuildConfigFromFlags(*MasterURL, *Kubeconfig)
	if err != nil {
		return nil, nil, err
	}
	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return nil, nil, err
	}
	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})

	return &stdout, &stderr, err
}

func getIstioVersion(client kubernetes.Interface, log *zap.SugaredLogger) (string, error) {
	cmds := []string{"/bin/bash", "-c",
		"env | grep ISTIO_META_ISTIO_VERSION | awk -F'=' '{print $2}'"}
	stdout, stderr, err := podExec(client, istioNamespace, istioPilotPodLabel,
		istioPilotContainer, cmds, log)

	log.Infof("podExec output: %s", stdout.String())
	if err != nil {
		if stderr.String() != "" {
			return "", fmt.Errorf("error execing get istio version: %v\n%s", stderr.String(), err)
		}
		return "", err
	}

	// remove extra characters from stdout such as "\n"
	ver := strings.TrimSuffix(stdout.String(), "\n")
	if !strings.HasPrefix(ver, "v") {
		ver = "v" + ver
	}
	return ver, nil
}

func validate(client kubernetes.Interface, t *depMeta, log *zap.SugaredLogger) error {
	basemsg := t.name + " validation"
	// check namespace created
	_, err := client.CoreV1().Namespaces().Get(t.namespace, metav1.GetOptions{})
	if err != nil {
		msg := t.namespace + " doesn't exist"
		log.Error(err, basemsg, "namespace checking", msg)
		return fmt.Errorf(msg)
	}

	// for istio, version could be get from env of container istio-proxy in pod istio-pilot
	// for cert-manager, it should be image tag which is confirmed by developer of cert-manager
	var ver string
	versionValid := false
	if t.name == istioDependency {
		ver, err = getIstioVersion(client, log)
		if err != nil {
			log.Error(err, basemsg+" version check failed")
			return err
		}

		err = checkDependencyVersion(t.name, ver, opversion.Istio, log)
		if err != nil {
			return err
		}
		versionValid = true
	}

	// check deployments installed successuflly and version satisfied with requirements
	for _, deployName := range t.deploy {
		deploy, err := client.ExtensionsV1beta1().Deployments(t.namespace).Get(deployName, metav1.GetOptions{})
		if err != nil {
			log.Error(err, basemsg, "deployment checking", "deployment")
			return fmt.Errorf("deployment %v is not available", deployName)
		}
		// check installed version
		if !versionValid {
			err = checkDeploymentVersion(deploy, t.name, log)
			if err != nil {
				return err
			}
			versionValid = true
		}

		if deploy.Status.Replicas != deploy.Status.ReadyReplicas {
			log.Debugf(basemsg, "ready replicas", deploy.Status.ReadyReplicas, "expected replicas", deploy.Status.Replicas)
			log.Debugf(basemsg, "deployment is not available", deployName)
			return fmt.Errorf("deployment %v is not available", deployName)
		}

		log.Debugf(basemsg, deployName, "Available")
	}

	log.Infof(basemsg + " succeeded")
	return nil
}

// ValidateDependency Firstly scan deps specified in CR to determin whether they're supported
// or not. Then perform dependency validation
func ValidateDependency(kclient kubernetes.Interface, deps []string, log *zap.SugaredLogger) error {
	log.Debugf("Dependency Validation started")
	// check k8s version
	if err := version.CheckMinimumVersion(kclient.Discovery()); err != nil {
		log.Error(err, "Failed to get k8s version")
		return err
	}
	log.Infof("Dependency Validation k8s version matched")

	var tasks []*depMeta
	log.Debugf("Dependency Validation", "deps", deps)
	for _, dep := range deps {
		if val, ok := depMetas[dep]; ok {
			tasks = append(tasks, &val)
		} else {
			return fmt.Errorf("dependency validator missed for %v", dep)
		}
	}

	for _, t := range tasks {
		err := validate(kclient, t, log)
		if err != nil {
			return err
		}
	}

	return nil
}
