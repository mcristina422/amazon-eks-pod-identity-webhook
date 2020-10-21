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

package cert

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	v1 "k8s.io/api/certificates/v1"

	"github.com/prometheus/client_golang/prometheus"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/certificate"
)

// NewServerCertificateManager returns a certificate manager that stores TLS keys in Kubernetes Secrets
func NewServerCertificateManager(kubeClient clientset.Interface, namespace, secretName string, csr *x509.CertificateRequest) (certificate.Manager, error) {
	clientFn := func(_ *tls.Certificate) (clientset.Interface, error) {
		return kubeClient, nil
	}

	certificateStore := NewSecretCertStore(
		namespace,
		secretName,
		kubeClient,
	)

	var certificateExpiration = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Subsystem: "certificate_manager",
			Name:      "server_expiration_seconds",
			Help:      "Gauge of the lifetime of a certificate. The value is the date the certificate will expire in seconds since January 1, 1970 UTC.",
		},
	)
	prometheus.MustRegister(certificateExpiration)

	m, err := certificate.NewManager(&certificate.Config{
		ClientsetFn: clientFn,
		Template: csr,
		Usages: []v1.KeyUsage{
			// https://tools.ietf.org/html/rfc5280#section-4.2.1.3
			//
			// Digital signature allows the certificate to be used to verify
			// digital signatures used during TLS negotiation.
			v1.UsageDigitalSignature,
			// KeyEncipherment allows the cert/key pair to be used to encrypt
			// keys, including the symmetric keys negotiated during TLS setup
			// and used for data transfer.
			v1.UsageKeyEncipherment,
			// ServerAuth allows the cert to be used by a TLS server to
			// authenticate itself to a TLS client.
			v1.UsageServerAuth,
		},
		CertificateStore:      certificateStore,
		//CertificateExpiration: certificateExpiration,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize server certificate manager: %v", err)
	}
	return m, nil
}
