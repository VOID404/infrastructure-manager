package auditlogs

import (
	"bytes"
	"slices"

	gardener "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/json"
)

const (
	auditlogExtensionType = "shoot-auditlog-service"
	auditlogReferenceName = "auditlog-credentials"
)

type AuditLogData struct {
	TenantID   string `json:"tenantID" validate:"required"`
	ServiceURL string `json:"serviceURL" validate:"required,url"`
	SecretName string `json:"secretName" validate:"required"`
}

type AuditlogExtensionConfig struct {
	metav1.TypeMeta `json:",inline"`
	// Type is the type of auditlog service provider.
	Type string `json:"type"`
	// TenantID is the id of the tenant.
	TenantID string `json:"tenantID"`
	// ServiceURL is the URL of the auditlog service.
	ServiceURL string `json:"serviceURL"`
	// SecretReferenceName is the name of the reference for the secret containing the auditlog service credentials.
	SecretReferenceName string `json:"secretReferenceName"`
}

func oSetExtension(d AuditLogData) operation {
	return func(s *gardener.Shoot) error {
		cfg := AuditlogExtensionConfig{
			TypeMeta: metav1.TypeMeta{
				Kind:       "AuditlogConfig",
				APIVersion: "service.auditlog.extensions.gardener.cloud/v1alpha1",
			},
			Type:                "standard",
			TenantID:            d.TenantID,
			ServiceURL:          d.ServiceURL,
			SecretReferenceName: auditlogReferenceName,
		}
		var buffer bytes.Buffer
		if err := json.NewEncoder(&buffer).Encode(&cfg); err != nil {
			return err
		}

		extension := gardener.Extension{
			Type: auditlogExtensionType,
			ProviderConfig: &runtime.RawExtension{
				Raw: buffer.Bytes(),
			},
		}

		index := slices.IndexFunc(s.Spec.Extensions, func(e gardener.Extension) bool {
			return e.Type == auditlogExtensionType
		})

		if index == -1 { // add extension
			s.Spec.Extensions = append(s.Spec.Extensions, extension)
			return nil
		}

		s.Spec.Extensions[index] = extension // update extension
		return nil
	}
}
