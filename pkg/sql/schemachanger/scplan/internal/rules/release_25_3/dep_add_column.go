// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package release_25_3

import (
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/rel"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	. "github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/rules"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/scgraph"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/screl"
)

// These rules ensure that column-dependent elements, like a column's name, its
// DEFAULT expression, etc. appear once the column reaches a suitable state.
func init() {

	registerDepRule(
		"column existence precedes column dependents",
		scgraph.Precedence,
		"column", "dependent",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.Column)(nil)),
				to.TypeFilter(rulesVersionKey, isColumnDependent),
				JoinOnColumnID(from, to, "table-id", "col-id"),
				StatusesToPublicOrTransient(from, scpb.Status_DELETE_ONLY, to, scpb.Status_PUBLIC),
				IsNotAlterColumnTypeOp("table-id", "col-id"),
			}
		},
	)

	// This rule is similar to the one above but omits the column name. This is specific
	// to ALTER COLUMN ... TYPE, which replaces a column with the same name. The column
	// name is assigned during the swap, as it replaces the old column.
	registerDepRule(
		"column existence precedes column dependents during an alter column type",
		scgraph.Precedence,
		"column", "dependent",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.Column)(nil)),
				to.TypeFilter(rulesVersionKey, isColumnDependentExceptColumnName),
				JoinOnColumnID(from, to, "table-id", "col-id"),
				StatusesToPublicOrTransient(from, scpb.Status_DELETE_ONLY, to, scpb.Status_PUBLIC),
				rel.And(IsAlterColumnTypeOp("table-id", "col-id")...),
			}
		},
	)

	registerDepRule(
		"column dependents exist before column becomes public",
		scgraph.Precedence,
		"dependent", "column",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.TypeFilter(rulesVersionKey, isColumnDependent),
				to.Type((*scpb.Column)(nil)),
				JoinOnColumnID(from, to, "table-id", "col-id"),
				StatusesToPublicOrTransient(from, scpb.Status_PUBLIC, to, scpb.Status_PUBLIC),
			}
		},
	)
}

// Special cases of the above.
func init() {
	registerDepRule(
		"column type set right after column existence",
		scgraph.SameStagePrecedence,
		"column", "column-type",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.Column)(nil)),
				to.Type((*scpb.ColumnType)(nil)),
				StatusesToPublicOrTransient(from, scpb.Status_DELETE_ONLY, to, scpb.Status_PUBLIC),
				JoinOnColumnID(from, to, "table-id", "col-id"),
			}
		},
	)

	// There are two distinct rules for transitioning the column name to public,
	// depending on whether an ALTER COLUMN TYPE operation is in progress.
	// This rule applies when an ALTER COLUMN TYPE is *not* occurring and is
	// identical to the prior rule for ColumnType, except that it applies to ColumnName.
	registerDepRule(
		"column name set right after column existence, except for alter column type",
		scgraph.SameStagePrecedence,
		"column", "column-name-or-type",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.Column)(nil)),
				to.Type((*scpb.ColumnName)(nil)),
				StatusesToPublicOrTransient(from, scpb.Status_DELETE_ONLY, to, scpb.Status_PUBLIC),
				JoinOnColumnID(from, to, "table-id", "col-id"),
				IsNotAlterColumnTypeOp("table-id", "col-id"),
			}
		},
	)

	// This rule is similar to the previous one but applies during an ALTER COLUMN TYPE operation.
	// In this scenario, we are replacing one column with another, both of which share the same name.
	// To prevent any period where a column with that name is not public, we ensure that the column
	// names are swapped in the same stage.
	registerDepRule(
		"during alter column type, column names for old and new columns are swapped in the same stage",
		scgraph.SameStagePrecedence,
		"old-column-name", "new-column-name",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.ColumnName)(nil)),
				to.Type((*scpb.ColumnName)(nil)),
				from.TargetStatus(scpb.ToAbsent),
				from.CurrentStatus(scpb.Status_ABSENT),
				to.TargetStatus(scpb.ToPublic),
				to.CurrentStatus(scpb.Status_PUBLIC),
				JoinOnDescID(from, to, "table-id"),
				to.El.AttrEqVar(screl.ColumnID, "new-col-id"),
				rel.And(IsAlterColumnTypeOp("table-id", "new-col-id")...),
			}
		},
	)

	registerDepRule(
		"DEFAULT or ON UPDATE existence precedes writes to column, except if they are added as part of a alter column type",
		scgraph.Precedence,
		"expr", "column",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type(
					(*scpb.ColumnDefaultExpression)(nil),
					(*scpb.ColumnOnUpdateExpression)(nil),
				),
				to.Type((*scpb.Column)(nil)),
				JoinOnColumnID(from, to, "table-id", "col-id"),
				IsNotAlterColumnTypeOp("table-id", "col-id"),
				StatusesToPublicOrTransient(from, scpb.Status_PUBLIC, to, scpb.Status_WRITE_ONLY),
			}
		},
	)

	// Column becomes writable in the same stage as column constraint is enforced.
	//
	// This rule exists to prevent the case that the constraint becomes enforced
	// (which means writes need to honor it) when the column itself is still
	// in DELETE_ONLY and thus not visible to writes.
	//
	// N.B. It's essentially the same rule as "column constraint removed right
	// before column reaches delete only" but on the adding path.
	// N.B. SameStage is enough; which transition happens first won't matter.
	registerDepRule(
		"column writable right before column constraint is enforced.",
		scgraph.SameStagePrecedence,
		"column", "column-constraint",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.Column)(nil)),
				to.Type((*scpb.ColumnNotNull)(nil)),
				JoinOnColumnID(from, to, "table-id", "col-id"),
				StatusesToPublicOrTransient(from, scpb.Status_WRITE_ONLY, to, scpb.Status_WRITE_ONLY),
			}
		},
	)

	// A computed expression cannot have a DEFAULT or ON UPDATE expression.
	// However, if the computed expression is temporary (e.g., for an ALTER COLUMN
	// TYPE requiring a backfill), these expressions can be added once the
	// computed expression is dropped.
	registerDepRule(
		"DEFAULT or ON UPDATE expressions is public after transient compute expression transitions to absent",
		scgraph.SameStagePrecedence,
		"transient-compute-expression", "column-expr",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.ColumnComputeExpression)(nil)),
				to.Type(
					(*scpb.ColumnDefaultExpression)(nil),
					(*scpb.ColumnOnUpdateExpression)(nil),
				),
				JoinOnColumnID(from, to, "table-id", "col-id"),
				ToPublicOrTransient(from, to),
				from.CurrentStatus(scpb.Status_TRANSIENT_ABSENT),
				to.CurrentStatus(scpb.Status_PUBLIC),
			}
		},
	)

	registerDepRule(
		"Final compute expression is always added after transient compute expression",
		scgraph.SameStagePrecedence,
		"transient-compute-expression", "final-compute-expression",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.ColumnComputeExpression)(nil)),
				to.Type((*scpb.ColumnComputeExpression)(nil)),
				JoinOnColumnID(from, to, "table-id", "col-id"),
				from.El.AttrEq(screl.Usage, scpb.ColumnComputeExpression_ALTER_TYPE_USING),
				to.El.AttrEq(screl.Usage, scpb.ColumnComputeExpression_REGULAR),
				ToPublicOrTransient(from, to),
				from.CurrentStatus(scpb.Status_TRANSIENT_ABSENT),
				to.CurrentStatus(scpb.Status_PUBLIC),
			}
		},
	)
}

// This rule ensures that columns depend on each other in increasing order.
func init() {
	registerDepRule(
		"ensure columns are in increasing order",
		scgraph.Precedence,
		"later-column", "earlier-column",
		func(from, to NodeVars) rel.Clauses {
			status := rel.Var("status")
			return rel.Clauses{
				from.Type((*scpb.Column)(nil)),
				// Join first on the target and node to only explore all columns
				// which are being added as opposed to all columns. If we joined
				// first on the columns, we'd be filtering the cross product of
				// table columns. If a relation has a lot of columns, this can hurt.
				// It's less likely that we have a very large number of columns which
				// are being added. We'll want to do something else here when we start
				// creating tables and all the columns are being added.
				//
				// The "right" answer is to push ordering predicates into rel; it also
				// is maintaining sorted data structures.
				from.JoinTargetNode(),
				to.Type((*scpb.Column)(nil)),
				JoinOnDescID(from, to, "table-id"),
				ToPublicOrTransient(from, to),
				status.In(scpb.Status_WRITE_ONLY, scpb.Status_PUBLIC),
				status.Entities(screl.CurrentStatus, from.Node, to.Node),
				FilterElements("SmallerColumnIDFirst", from, to, func(from, to *scpb.Column) bool {
					return from.ColumnID < to.ColumnID
				}),
			}
		})
}
