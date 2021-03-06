/*
  Copyright 2019 Amazon.com, Inc. or its affiliates. All Rights Reserved.

  Licensed under the Apache License, Version 2.0 (the "License").
  You may not use this file except in compliance with the License.
  A copy of the License is located at

      http://www.apache.org/licenses/LICENSE-2.0

  or in the "license" file accompanying this file. This file is distributed
  on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
  express or implied. See the License for the specific language governing
  permissions and limitations under the License.
*/

package handler

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/aws/amazon-eks-pod-identity-webhook/pkg"
	"github.com/aws/amazon-eks-pod-identity-webhook/pkg/cache"
	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/apis/core/v1"
)

type podUpdateSettings struct {
	skipContainers map[string]bool
	useRegionalSTS bool
}

// newPodUpdateSettings returns the update settings for a particular pod
func newPodUpdateSettings(annotationDomain string, pod *corev1.Pod, useRegionalSTS bool) *podUpdateSettings {
	settings := &podUpdateSettings{
		useRegionalSTS: useRegionalSTS,
	}

	skippedNames := map[string]bool{}
	skipContainersKey := annotationDomain + "/" + pkg.SkipContainersAnnotation
	if value, ok := pod.Annotations[skipContainersKey]; ok {
		r := csv.NewReader(strings.NewReader(value))
		// error means we don't skip any
		podNames, err := r.Read()
		if err != nil {
			klog.Infof("Could parse skip containers annotation on pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		for _, name := range podNames {
			skippedNames[name] = true
		}
	}
	settings.skipContainers = skippedNames
	return settings
}

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionregistrationv1beta1.AddToScheme(runtimeScheme)
	_ = v1.AddToScheme(runtimeScheme)
}

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

// ModifierOpt is an option type for setting up a Modifier
type ModifierOpt func(*Modifier)

// WithServiceAccountCache sets the modifiers cache
func WithServiceAccountCache(c cache.ServiceAccountCache) ModifierOpt {
	return func(m *Modifier) { m.Cache = c }
}

// WithMountPath sets the modifier mountPath
func WithMountPath(mountpath string) ModifierOpt {
	return func(m *Modifier) { m.MountPath = mountpath }
}

// WithExpiration sets the modifier expiration
func WithExpiration(exp int64) ModifierOpt {
	return func(m *Modifier) { m.Expiration = exp }
}

// WithRegion sets the modifier region
func WithRegion(region string) ModifierOpt {
	return func(m *Modifier) { m.Region = region }
}

// WithRegionalSTS sets the modifier RegionalSTSEndpoint
func WithRegionalSTS(enabled bool) ModifierOpt {
	return func(m *Modifier) { m.RegionalSTSEndpoint = enabled }
}

// WithAnnotationDomain adds an annotation domain
func WithAnnotationDomain(domain string) ModifierOpt {
	return func(m *Modifier) { m.AnnotationDomain = domain }
}

// NewModifier returns a Modifier with default values
func NewModifier(opts ...ModifierOpt) *Modifier {
	mod := &Modifier{
		AnnotationDomain:    "eks.amazonaws.com",
		MountPath:           "/var/run/secrets/eks.amazonaws.com/serviceaccount",
		Expiration:          86400,
		RegionalSTSEndpoint: false,
		volName:             "aws-iam-token",
		tokenName:           "token",
	}
	for _, opt := range opts {
		opt(mod)
	}

	return mod
}

// Modifier holds configuration values for pod modifications
type Modifier struct {
	AnnotationDomain    string
	Expiration          int64
	MountPath           string
	Region              string
	RegionalSTSEndpoint bool
	Cache               cache.ServiceAccountCache
	volName             string
	tokenName           string
}

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func logContext(podName, podGenerateName, serviceAccountName, namespace string) string {
	name := podName
	if len(podName) == 0 {
		name = podGenerateName
	}
	return fmt.Sprintf("Pod=%s, " +
		"ServiceAccount=%s, " +
		"Namespace=%s",
		name,
		serviceAccountName,
		namespace)
}

func (m *Modifier) addEnvToContainer(container *corev1.Container, tokenFilePath, roleName string, podSettings *podUpdateSettings) {
	// return if this is a named skipped container
	if _, ok := podSettings.skipContainers[container.Name]; ok {
		return
	}

	var (
		reservedKeysDefined   bool
		regionKeyDefined      bool
		regionalStsKeyDefined bool
	)
	reservedKeys := map[string]string{
		"AWS_ROLE_ARN":                "",
		"AWS_WEB_IDENTITY_TOKEN_FILE": "",
	}
	awsRegionKeys := map[string]string{
		"AWS_REGION":         "",
		"AWS_DEFAULT_REGION": "",
	}
	stsKey := "AWS_STS_REGIONAL_ENDPOINTS"
	for _, env := range container.Env {
		if _, ok := reservedKeys[env.Name]; ok {
			reservedKeysDefined = true
		}
		if _, ok := awsRegionKeys[env.Name]; ok {
			// Don't set both region keys if any region key is already set
			regionKeyDefined = true
		}
		if env.Name == stsKey {
			regionalStsKeyDefined = true
		}
	}

	if reservedKeysDefined && regionKeyDefined && regionalStsKeyDefined {
		return
	}

	env := container.Env

	if !regionalStsKeyDefined && m.RegionalSTSEndpoint && podSettings.useRegionalSTS {
		env = append(env,
			corev1.EnvVar{
				Name:  stsKey,
				Value: "regional",
			},
		)
	}

	if !regionKeyDefined && m.Region != "" {
		env = append(env,
			corev1.EnvVar{
				Name:  "AWS_DEFAULT_REGION",
				Value: m.Region,
			},
			corev1.EnvVar{
				Name:  "AWS_REGION",
				Value: m.Region,
			},
		)
	}

	if !reservedKeysDefined {
		env = append(env, corev1.EnvVar{
			Name:  "AWS_ROLE_ARN",
			Value: roleName,
		})

		env = append(env, corev1.EnvVar{
			Name:  "AWS_WEB_IDENTITY_TOKEN_FILE",
			Value: tokenFilePath,
		})
	}

	container.Env = env

	volExists := false
	for _, vol := range container.VolumeMounts {
		if vol.Name == m.volName {
			volExists = true
		}
	}

	if !volExists {
		container.VolumeMounts = append(
			container.VolumeMounts,
			corev1.VolumeMount{
				Name:      m.volName,
				ReadOnly:  true,
				MountPath: m.MountPath,
			},
		)
	}
}

func (m *Modifier) updatePodSpec(pod *corev1.Pod, roleName, audience string, regionalSTS bool) []patchOperation {
	updateSettings := newPodUpdateSettings(m.AnnotationDomain, pod, regionalSTS)

	tokenFilePath := filepath.Join(m.MountPath, m.tokenName)

	betaNodeSelector, _ := pod.Spec.NodeSelector["beta.kubernetes.io/os"]
	nodeSelector, _ := pod.Spec.NodeSelector["kubernetes.io/os"]
	if (betaNodeSelector == "windows") || nodeSelector == "windows" {
		// Convert the unix file path to a windows file path
		// Eg. /var/run/secrets/eks.amazonaws.com/serviceaccount/token to
		//     C:\var\run\secrets\eks.amazonaws.com\serviceaccount\token
		tokenFilePath = "C:" + strings.Replace(tokenFilePath, `/`, `\`, -1)
	}

	var initContainers = []corev1.Container{}
	for i := range pod.Spec.InitContainers {
		container := pod.Spec.InitContainers[i]
		m.addEnvToContainer(&container, tokenFilePath, roleName, updateSettings)
		initContainers = append(initContainers, container)
	}
	var containers = []corev1.Container{}
	for i := range pod.Spec.Containers {
		container := pod.Spec.Containers[i]
		m.addEnvToContainer(&container, tokenFilePath, roleName, updateSettings)
		containers = append(containers, container)
	}

	volume := corev1.Volume{
		Name: m.volName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          audience,
							ExpirationSeconds: &m.Expiration,
							Path:              m.tokenName,
						},
					},
				},
			},
		},
	}

	patch := []patchOperation{}

	// skip adding volume if it already exists
	volExists := false
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == m.volName {
			volExists = true
		}
	}

	if !volExists {
		volPatch := patchOperation{
			Op:    "add",
			Path:  "/spec/volumes/0",
			Value: volume,
		}

		if pod.Spec.Volumes == nil {
			volPatch = patchOperation{
				Op:   "add",
				Path: "/spec/volumes",
				Value: []corev1.Volume{
					volume,
				},
			}
		}

		patch = append(patch, volPatch)
	}

	patch = append(patch, patchOperation{
		Op:    "add",
		Path:  "/spec/containers",
		Value: containers,
	})

	if len(initContainers) > 0 {
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  "/spec/initContainers",
			Value: initContainers,
		})
	}
	return patch
}

// MutatePod takes a AdmissionReview, mutates the pod, and returns an AdmissionResponse
func (m *Modifier) MutatePod(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	badRequest := &v1beta1.AdmissionResponse{
		Result: &metav1.Status{
			Message: "bad content",
		},
	}
	if ar == nil {
		return badRequest
	}
	req := ar.Request
	if req == nil {
		return badRequest
	}

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		klog.Errorf("Could not unmarshal raw object: %v", err)
		klog.Errorf("Object: %v", string(req.Object.Raw))
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	pod.Namespace = req.Namespace

	podRole, audience, regionalSTS := m.Cache.Get(pod.Spec.ServiceAccountName, pod.Namespace)

	// determine whether to perform mutation
	if podRole == "" {
		klog.V(3).Infof("Pod was not mutated. %s",
			logContext(pod.Name, pod.GenerateName, pod.Spec.ServiceAccountName, pod.Namespace))
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	}

	patchBytes, err := json.Marshal(m.updatePodSpec(&pod, podRole, audience, regionalSTS))
	if err != nil {
		klog.Errorf("Error marshaling pod update: %v", err.Error())
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	klog.V(3).Infof("Pod was mutated. %s",
		logContext(pod.Name, pod.GenerateName, pod.Spec.ServiceAccountName, pod.Namespace))
	return &v1beta1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *v1beta1.PatchType {
			pt := v1beta1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

// Handle handles pod modification requests
func (m *Modifier) Handle(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		klog.Errorf("Content-Type=%s, expected application/json", contentType)
		http.Error(w, "Invalid Content-Type, expected `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	var admissionResponse *v1beta1.AdmissionResponse
	ar := v1beta1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		klog.Errorf("Can't decode body: %v", err)
		admissionResponse = &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	} else {
		admissionResponse = m.MutatePod(&ar)
	}

	admissionReview := v1beta1.AdmissionReview{}
	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		if ar.Request != nil {
			admissionReview.Response.UID = ar.Request.UID
		}
	}

	resp, err := json.Marshal(admissionReview)
	if err != nil {
		klog.Errorf("Can't encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	if _, err := w.Write(resp); err != nil {
		klog.Errorf("Can't write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}
