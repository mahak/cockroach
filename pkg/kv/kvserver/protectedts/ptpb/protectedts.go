// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ptpb

import (
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
)

// MakeClusterTarget returns a target, which when used in a Record, will
// protect the entire keyspace of the cluster.
func MakeClusterTarget() *Target {
	return &Target{Union: &Target_Cluster{Cluster: &Target_ClusterTarget{}}}
}

// MakeTenantsTarget returns a target, which when used in a Record, will
// protect the keyspace of all tenants in ids.
func MakeTenantsTarget(ids []roachpb.TenantID) *Target {
	return &Target{Union: &Target_Tenants{Tenants: &Target_TenantsTarget{IDs: ids}}}
}

// MakeSchemaObjectsTarget returns a target, which when used in a Record,
// will protect the keyspace of all schema objects (database/table).
func MakeSchemaObjectsTarget(ids descpb.IDs) *Target {
	return &Target{Union: &Target_SchemaObjects{SchemaObjects: &Target_SchemaObjectsTarget{IDs: ids}}}
}
