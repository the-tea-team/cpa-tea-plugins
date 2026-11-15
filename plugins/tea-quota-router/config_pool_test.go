package main

import "testing"

func TestConfigPoolCamelCaseFields(t *testing.T) {
	c, e := parseConfig([]byte("dataDir: /unused\npools:\n - id: claude-pool\n   provider: claude\n   billingType: subscription\n   credentialIds: [c1]\n"))
	if e != nil || len(c.Pools) != 1 || c.Pools[0].BillingType != "subscription" || len(c.Pools[0].CredentialIDs) != 1 {
		t.Fatalf("config lost pool fields: %+v %v", c.Pools, e)
	}
}
