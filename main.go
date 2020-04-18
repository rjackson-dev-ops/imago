/*
Copyright 2019 Philippe Pepiot <phil@philpep.org>


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
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

func closeResource(r io.Closer) {
	err := r.Close()
	if err != nil {
		log.Print(err)
	}
}

// splitDockerDomain splits a repository name to domain and remotename string.
// If no valid domain is found, the default domain is used. Repository name
// needs to be already validated before.
// From https://github.com/docker/distribution/blob/master/reference/normalize.go
func splitDockerDomain(name string) (domain, remainder string) {
	defaultDomain := "registry.hub.docker.com"
	legacyDefaultDomain := "index.docker.io"
	i := strings.IndexRune(name, '/')
	if i == -1 || (!strings.ContainsAny(name[:i], ".:") && name[:i] != "localhost") {
		domain, remainder = defaultDomain, name
	} else {
		domain, remainder = name[:i], name[i+1:]
	}
	if domain == legacyDefaultDomain {
		domain = defaultDomain
	}
	if domain == defaultDomain && !strings.ContainsRune(remainder, '/') {
		remainder = "library" + "/" + remainder
	}
	return
}

func getDigestURL(name string) (string, error) {
	domain, image := splitDockerDomain(name)
	tag := "latest"
	if strings.Contains(image, ":") {
		s := strings.Split(image, ":")
		image, tag = s[0], s[1]
	}
	return fmt.Sprintf("https://%s/v2/%s/manifests/%s", domain, image, tag), nil
}

func getBearerToken(client *http.Client, authHeader string) (string, error) {
	r := regexp.MustCompile("(.*)=\"(.*)\"")
	authInfo := make(map[string]string)
	for _, part := range strings.Split(strings.SplitN(authHeader, " ", 2)[1], ",") {
		match := r.FindStringSubmatch(part)
		authInfo[match[1]] = match[2]
	}
	if authInfo["realm"] == "" || authInfo["service"] == "" || authInfo["scope"] == "" {
		return "", fmt.Errorf("unexpected or missing auth headers: %s", authInfo)
	}
	req, err := http.NewRequest("GET", authInfo["realm"], nil)
	if err != nil {
		return "", err
	}
	q := req.URL.Query()
	q.Add("service", authInfo["service"])
	q.Add("scope", authInfo["scope"])
	req.URL.RawQuery = q.Encode()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer closeResource(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("error while requesting auth token on %s: %s", req.URL, resp.Status)
	}
	var result struct {
		Token string `json:"token"`
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	err = json.Unmarshal(body, &result)
	if err != nil {
		return "", err
	}
	return result.Token, nil
}

// RegistryClient represent a docker client
type RegistryClient struct {
	client      *http.Client
	Auth        map[string]string
	DefaultAuth map[string]string
	cache       map[string]string
}

// DockerRegistryCredentials represent content of docker config.json file
type DockerRegistryCredentials struct {
	Auths map[string]struct {
		Auth string `json:"auth"`
	} `json:"auths"`
}

// NewRegistryClient initialize a RegistryClient
func NewRegistryClient(client *http.Client) *RegistryClient {
	if client == nil {
		client = &http.Client{
			Timeout: time.Second * 10,
		}
	}
	return &RegistryClient{
		client: client,
		Auth:   make(map[string]string),
		cache:  make(map[string]string)}
}

// GetDigest return the docker digest of given image name
func (c *RegistryClient) GetDigest(name string) (string, error) {
	digestURL, err := getDigestURL(name)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest("HEAD", digestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v1+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v1+prettyjws")
	u, err := url.Parse(digestURL)
	if err != nil {
		return "", err
	}
	if c.Auth[u.Host] != "" {
		req.Header.Add("Authorization", fmt.Sprintf("Basic %s", c.Auth[u.Host]))
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer closeResource(resp.Body)
	authenticate := resp.Header.Get("www-authenticate")
	if resp.StatusCode == 401 && strings.HasPrefix(authenticate, "Bearer ") {
		token, err := getBearerToken(c.client, authenticate)
		if err != nil {
			return "", err
		}
		req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", token))
		resp, err = c.client.Do(req)
		if err != nil {
			return "", err
		}
		defer closeResource(resp.Body)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("unexpected response while requesting %s: %s", digestURL, resp.Status)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("no Docker-Content-Digest in response headers for %s", digestURL)
	}
	return digest, nil
}

// Config represent a imago configuration
type Config struct {
	cluster     *kubernetes.Clientset
	reg         *RegistryClient
	secretCache map[string]*v1.Secret
	namespace   string
	update      bool
	checkpods   bool
	xnamespace  *arrayFlags
}

// NewConfig initialize a new imago config
func NewConfig(kubeconfig string, namespace string, allnamespaces bool, xnamespace *arrayFlags, update bool, checkpods bool, dockerconfig string) (*Config, error) {
	c := &Config{reg: NewRegistryClient(nil), update: update, checkpods: checkpods, xnamespace: xnamespace}
	var err error
	var clusterConfig *rest.Config

	setNamespace := func(incluster bool) error {
		if allnamespaces {
			c.namespace = ""
		} else if namespace != "" {
			c.namespace = namespace
		} else {
			if incluster {
				c.namespace = inClusterNamespace()
			} else {
				c.namespace = outClusterNamespace(kubeconfig)
			}
			if c.namespace == "" {
				c.namespace = "default"
			}
		}
		return nil
	}

	if inClusterClientPossible() {
		clusterConfig, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
		if err = setNamespace(true); err != nil {
			return nil, err
		}
	} else {
		clusterConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
		if err = setNamespace(false); err != nil {
			return nil, err
		}
	}
	c.cluster, err = kubernetes.NewForConfig(clusterConfig)
	if err != nil {
		return nil, err
	}
	var dockerconfigjson DockerRegistryCredentials
	var data []byte
	c.reg.DefaultAuth = make(map[string]string)
	if dockerconfig == "" {
		dockerconfig = filepath.Join(homeDir(), ".docker", "config.json")
		data, err = ioutil.ReadFile(dockerconfig)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		data, err = ioutil.ReadFile(dockerconfig)
		if err != nil {
			return nil, err
		}
	}
	if len(data) > 0 {
		err = json.Unmarshal(data, &dockerconfigjson)
		if err != nil {
			return nil, err
		}
		for host, auth := range dockerconfigjson.Auths {
			c.reg.DefaultAuth[host] = auth.Auth
		}
	}
	return c, nil
}

// Update Deployment, DaemonSet and CronJob matching given selectors
func (c *Config) Update(fieldSelector, labelSelector string) error {
	client := c.cluster.AppsV1()
	opts := metav1.ListOptions{FieldSelector: fieldSelector, LabelSelector: labelSelector}
	deployments, err := client.Deployments(c.namespace).List(opts)
	if err != nil {
		return err
	}
	failed := make([]string, 0)
	for _, d := range deployments.Items {
		if err = c.setImages("Deployment", &d.ObjectMeta, &d.Spec.Template); err != nil {
			log.Print(err)
			failed = append(failed, fmt.Sprintf("failed to check %s/Deployment/%s: %s", d.ObjectMeta.Namespace, d.Name, err))
		}
	}
	daemonsets, err := client.DaemonSets(c.namespace).List(opts)
	if err != nil {
		return err
	}
	for _, ds := range daemonsets.Items {
		if err := c.setImages("DaemonSet", &ds.ObjectMeta, &ds.Spec.Template); err != nil {
			failed = append(failed, fmt.Sprintf("failed to check %s/DaemonSet/%s: %s", ds.ObjectMeta.Namespace, ds.Name, err))
		}
	}
	statefulsets, err := client.StatefulSets(c.namespace).List(opts)
	if err != nil {
		return err
	}
	for _, sts := range statefulsets.Items {
		if err := c.setImages("StatefulSet", &sts.ObjectMeta, &sts.Spec.Template); err != nil {
			failed = append(failed, fmt.Sprintf("failed to check %s/StatefulSet/%s: %s", sts.ObjectMeta.Namespace, sts.Name, err))
		}
	}
	batchClient := c.cluster.BatchV1beta1()
	cronjobs, err := batchClient.CronJobs(c.namespace).List(opts)
	if err != nil {
		return err
	}
	for _, cron := range cronjobs.Items {
		if err := c.setImages("CronJob", &cron.ObjectMeta, &cron.Spec.JobTemplate.Spec.Template); err != nil {
			failed = append(failed, fmt.Sprintf("failed to check %s/CronJob/%s: %s", cron.ObjectMeta.Namespace, cron.Name, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf(strings.Join(failed, "\n"))
	}
	return nil
}

func (c *Config) getSecret(namespace string, name string) (*v1.Secret, error) {
	key := fmt.Sprintf("%s/%s", namespace, name)
	if c.secretCache == nil {
		c.secretCache = make(map[string]*v1.Secret)
	}
	if c.secretCache[key] == nil {
		secret, err := c.cluster.CoreV1().Secrets(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		c.secretCache[key] = secret
	}
	return c.secretCache[key], nil
}

func (c *Config) setRegistryCredentials(namespace string, secrets []v1.LocalObjectReference) error {
	c.reg.Auth = make(map[string]string)
	for k, v := range c.reg.DefaultAuth {
		c.reg.Auth[k] = v
	}
	var dockerconfig DockerRegistryCredentials
	for _, secret := range secrets {
		secret, err := c.getSecret(namespace, secret.Name)
		if err != nil {
			return err
		}
		err = json.Unmarshal(secret.Data[v1.DockerConfigJsonKey], &dockerconfig)
		if err != nil {
			return err
		}
		for host, auth := range dockerconfig.Auths {
			c.reg.Auth[host] = auth.Auth
		}
	}
	return nil
}

type configAnnotationImageSpec struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type configAnnotation struct {
	Containers     []configAnnotationImageSpec `json:"containers"`
	InitContainers []configAnnotationImageSpec `json:"initContainers"`
}

const imagoConfigAnnotation = "imago-config-spec"

func mergeContainers(configContainers []configAnnotationImageSpec, containers []v1.Container) []configAnnotationImageSpec {
	specImages := make(map[string]string)
	for _, c := range containers {
		specImages[c.Name] = c.Image
	}
	re := regexp.MustCompile(".*@(sha256:.*)")
	configImages := make(map[string]string)
	for _, c := range configContainers {
		// drop containers in spec but not in config
		image := specImages[c.Name]
		if image != "" {
			match := re.FindStringSubmatch(image)
			if len(match) > 1 {
				// keep stored config
				configImages[c.Name] = c.Image
			} else {
				// use newer image
				configImages[c.Name] = specImages[c.Name]
			}
		}
	}
	for name, image := range specImages {
		if configImages[name] == "" {
			configImages[name] = image
		}
	}
	result := make([]configAnnotationImageSpec, 0)
	for name, image := range configImages {
		result = append(result, configAnnotationImageSpec{
			Name: name, Image: image})
	}
	return result
}

func getConfigAnnotation(meta *metav1.ObjectMeta, spec *v1.PodSpec) (*configAnnotation, error) {
	config := configAnnotation{}
	rawConfig := meta.GetAnnotations()[imagoConfigAnnotation]
	if len(rawConfig) > 0 {
		err := json.Unmarshal([]byte(rawConfig), &config)
		if err != nil {
			return nil, err
		}
	}
	config.Containers = mergeContainers(config.Containers, spec.Containers)
	config.InitContainers = mergeContainers(config.InitContainers, spec.InitContainers)
	return &config, nil
}

func needUpdate(name string, image string, specImage string, running map[string]string) bool {
	if len(running) == 0 {
		if image != specImage {
			log.Printf("    %s need to be updated from %s to %s", name, specImage, image)
			return true
		}
		log.Printf("    %s ok", name)
		return false
	}
	result := false
	for pod, digest := range running {
		if digest != image {
			log.Printf("    %s on %s need to be updated from %s to %s", name, pod, digest, image)
			result = true
		} else {
			log.Printf("    %s on %s ok", name, pod)
		}
	}
	return result
}

func (c *Config) getUpdates(configContainers []configAnnotationImageSpec, containers []v1.Container, running map[string]map[string]string) map[string]string {
	re := regexp.MustCompile(".*@(sha256:.*)")
	update := make(map[string]string)
	for _, container := range configContainers {
		match := re.FindStringSubmatch(container.Image)
		if len(match) > 1 {
			log.Printf("    %s ok (fixed digest)", container.Name)
			continue
		}
		digest, err := c.reg.GetDigest(container.Image)
		if err != nil {
			log.Printf("    %s unable to get digest: %s", container.Name, err)
			continue
		}
		image := strings.Split(container.Image, ":")[0] + "@" + digest
		for _, specContainer := range containers {
			if specContainer.Name != container.Name {
				continue
			}
			if needUpdate(container.Name, image, specContainer.Image, running[container.Name]) {
				update[container.Name] = image
			}
		}
	}
	return update
}

func getSelector(labels map[string]string) string {
	filters := make([]string, 0)
	for key, value := range labels {
		filters = append(filters, fmt.Sprintf("%s=%s", key, value))
	}
	return strings.Join(filters, ", ")
}

func (c *Config) getRunningContainers(kind string, meta *metav1.ObjectMeta, template *v1.PodTemplateSpec) (map[string]map[string]string, map[string]map[string]string, error) {
	runningInitContainers, runningContainers := make(map[string]map[string]string), make(map[string]map[string]string)
	if !c.checkpods {
		return runningInitContainers, runningContainers, nil
	}
	labelSelector := getSelector(template.ObjectMeta.Labels)
	running, err := c.cluster.CoreV1().Pods(meta.Namespace).List(metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return runningInitContainers, runningContainers, err
	}
	match := func(pod *v1.Pod) bool {
		for _, owner := range pod.OwnerReferences {
			switch owner.Kind {
			case "ReplicaSet":
				rs, err := c.cluster.AppsV1().ReplicaSets(meta.Namespace).Get(owner.Name, metav1.GetOptions{})
				if err != nil {
					log.Print(err)
					continue
				}
				for _, rsOwner := range rs.OwnerReferences {
					if rsOwner.Kind == kind && rsOwner.Name == meta.Name {
						return true
					}
				}
			case "DaemonSet":
				if owner.Kind == kind && owner.Name == meta.Name {
					return true
				}
			case "StatefulSet":
				if owner.Kind == kind && owner.Name == meta.Name {
					return true
				}
			}
		}
		return false
	}
	re := regexp.MustCompile(".*://(.*@sha256:.*)")
	addImage := func(containers map[string]map[string]string, name string, podName string, image string) {
		reMatch := re.FindStringSubmatch(image)
		if len(reMatch) < 2 {
			log.Printf("Unable to parse image digest %s", image)
			return
		}
		if containers[name] == nil {
			containers[name] = make(map[string]string)
		}
		containers[name][podName] = reMatch[1]
	}
	for _, pod := range running.Items {
		if match(&pod) {
			runningInitContainers[pod.Name] = make(map[string]string)
			runningContainers[pod.Name] = make(map[string]string)
			for _, container := range pod.Status.InitContainerStatuses {
				addImage(runningInitContainers, container.Name, pod.Name, container.ImageID)
			}
			for _, container := range pod.Status.ContainerStatuses {
				addImage(runningContainers, container.Name, pod.Name, container.ImageID)
			}
		}
	}
	return runningInitContainers, runningContainers, nil
}

func (c *Config) setImages(kind string, meta *metav1.ObjectMeta, template *v1.PodTemplateSpec) error {
	if c.xnamespace.Contains(meta.Namespace) {
		// namespace excluded from selection
		return nil
	}
	log.Printf("checking %s/%s/%s", meta.Namespace, kind, meta.Name)
	err := c.setRegistryCredentials(meta.Namespace, template.Spec.ImagePullSecrets)
	if err != nil {
		return err
	}
	config, err := getConfigAnnotation(meta, &template.Spec)
	if err != nil {
		return err
	}
	runningInitContainers, runningContainers, err := c.getRunningContainers(kind, meta, template)
	if err != nil {
		return err
	}
	updateInitContainers := c.getUpdates(config.InitContainers, template.Spec.InitContainers, runningInitContainers)
	updateContainers := c.getUpdates(config.Containers, template.Spec.Containers, runningContainers)
	if !c.update || (len(updateContainers) == 0 && len(updateInitContainers) == 0) {
		return nil
	}
	log.Printf("update %s/%s/%s", meta.Namespace, kind, meta.Name)
	jsonConfig, err := json.Marshal(config)
	if err != nil {
		return err
	}
	jsonConfigString := string(jsonConfig)
	var setAnnotation = func(meta *metav1.ObjectMeta) {
		if meta.Annotations == nil {
			meta.Annotations = make(map[string]string)
		}
		meta.Annotations[imagoConfigAnnotation] = jsonConfigString
	}
	var updateSpec = func(containers []v1.Container, update map[string]string) {
		for i, container := range containers {
			if newImage, ok := update[container.Name]; ok {
				containers[i].Image = newImage
			}
		}
	}
	var updateResource func() error
	switch kind {
	case "Deployment":
		updateResource = func() error {
			client := c.cluster.AppsV1().Deployments(meta.Namespace)
			resource, err := client.Get(meta.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			setAnnotation(&resource.ObjectMeta)
			updateSpec(resource.Spec.Template.Spec.Containers, updateContainers)
			updateSpec(resource.Spec.Template.Spec.InitContainers, updateInitContainers)
			_, err = client.Update(resource)
			return err
		}
	case "DaemonSet":
		updateResource = func() error {
			client := c.cluster.AppsV1().DaemonSets(meta.Namespace)
			resource, err := client.Get(meta.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			setAnnotation(&resource.ObjectMeta)
			updateSpec(resource.Spec.Template.Spec.Containers, updateContainers)
			updateSpec(resource.Spec.Template.Spec.InitContainers, updateInitContainers)
			_, err = client.Update(resource)
			return err
		}
	case "StatefulSet":
		updateResource = func() error {
			client := c.cluster.AppsV1().StatefulSets(meta.Namespace)
			resource, err := client.Get(meta.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			setAnnotation(&resource.ObjectMeta)
			updateSpec(resource.Spec.Template.Spec.Containers, updateContainers)
			updateSpec(resource.Spec.Template.Spec.InitContainers, updateInitContainers)
			_, err = client.Update(resource)
			return err
		}
	case "CronJob":
		updateResource = func() error {
			client := c.cluster.BatchV1beta1().CronJobs(meta.Namespace)
			resource, err := client.Get(meta.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			setAnnotation(&resource.ObjectMeta)
			updateSpec(resource.Spec.JobTemplate.Spec.Template.Spec.Containers, updateContainers)
			updateSpec(resource.Spec.JobTemplate.Spec.Template.Spec.InitContainers, updateInitContainers)
			_, err = client.Update(resource)
			return err
		}
	default:
		return fmt.Errorf("unhandled kind %s", kind)
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, updateResource); err != nil {
		return err
	}
	return nil
}

func inClusterClientPossible() bool {
	fi, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token")
	return os.Getenv("KUBERNETES_SERVICE_HOST") != "" &&
		os.Getenv("KUBERNETES_SERVICE_PORT") != "" &&
		err == nil && !fi.IsDir()
}

func defaultKubeConfig() string {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(homeDir(), ".kube", "config")
	}
	return kubeconfig
}

func inClusterNamespace() string {
	data, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		log.Fatal(err)
	}
	if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
		return ns
	}
	return ""
}

func outClusterNamespace(kubeconfig string) string {
	config := clientcmd.GetConfigFromFileOrDie(kubeconfig)
	if len(config.Contexts) == 0 || config.Contexts[config.CurrentContext] == nil {
		log.Fatal("No kubernetes contexts availables")
	}
	return config.Contexts[config.CurrentContext].Namespace
}

func homeDir() string {
	user, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	return user.HomeDir
}

type arrayFlags []string

func (i *arrayFlags) String() string {
	return ""
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func (i *arrayFlags) Contains(value string) bool {
	for _, x := range *i {
		if x == value {
			return true
		}
	}
	return false
}

func main() {
	var kubeconfig string
	var labelSelector string
	var fieldSelector string
	var allnamespaces bool
	var namespace arrayFlags
	var xnamespace arrayFlags
	var update bool
	var checkpods bool
	var dockerconfig string
	flag.StringVar(&kubeconfig, "kubeconfig", defaultKubeConfig(), "kube config file")
	flag.Var(&namespace, "n", "Check deployments and daemonsets in given namespaces (default to current namespace)")
	flag.Var(&xnamespace, "x", "Check deployments and daemonsets in all namespaces except given namespaces (implies --all-namespaces)")
	flag.StringVar(&labelSelector, "l", "", "Kubernetes labels selectors\nWarning: applies to Deployment, DaemonSet, StatefulSet and CronJob, not pods !")
	flag.StringVar(&fieldSelector, "field-selector", "", "Kubernetes field-selector\nexample: metadata.name=myapp")
	flag.BoolVar(&allnamespaces, "all-namespaces", false, "Check deployments and daemonsets on all namespaces (default false)")
	flag.BoolVar(&allnamespaces, "A", false, "Check deployments and daemonsets on all namespaces (shorthand) (default false)")
	flag.BoolVar(&update, "update", false, "update deployments and daemonsets to use newer images (default false)")
	flag.BoolVar(&checkpods, "check-pods", false, "check image digests of running pods (default false)")
	flag.StringVar(&dockerconfig, "docker-config", "", "docker config file for pulling latest digests (default ~/.docker/config.json)")
	flag.Parse()
	if allnamespaces && len(namespace) > 0 {
		log.Fatal("You can't use -n with --all-namespaces")
	}
	if len(namespace) == 0 {
		namespace = append(namespace, "")
	}
	if len(xnamespace) > 0 {
		allnamespaces = true
	}
	for _, ns := range namespace {
		c, err := NewConfig(kubeconfig, ns, allnamespaces, &xnamespace, update, checkpods, dockerconfig)
		if err != nil {
			log.Fatal(err)
		}
		if err := c.Update(fieldSelector, labelSelector); err != nil {
			log.Fatal(err)
		}
	}
}
