package auditlogs

import (
	gardener "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	imv1 "github.com/kyma-project/infrastructure-manager/api/v1"
)

type Extend = func(runtime imv1.Runtime, shoot *gardener.Shoot) error

type operation = func(*gardener.Shoot) error

func NewAuditlogExtenderForCreate(policyConfigMapName string, data AuditLogData) Extend {
	return func(_ imv1.Runtime, shoot *gardener.Shoot) error {
		for _, f := range []operation{
			oSetSecret(data.SecretName),
			oSetPolicyConfigmap(policyConfigMapName),
		} {
			if err := f(shoot); err != nil {
				return err
			}
		}
		return nil
	}
}

func NewAuditlogExtenderForPatch(policyConfigMapName string) Extend {
	return func(_ imv1.Runtime, shoot *gardener.Shoot) error {
		return oSetPolicyConfigmap(policyConfigMapName)(shoot)
	}
}
