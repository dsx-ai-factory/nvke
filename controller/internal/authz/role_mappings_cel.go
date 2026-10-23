// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"fmt"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/ext"
)

const (
	// spiffeCELPathVar is the one variable a spiffe mapping's expression sees:
	// the verified SPIFFE ID's path. Never the raw subject and never the trust
	// domain, so an expression cannot scope on the domain even by mistake; the
	// trust-domain check runs in Go before the expression does.
	spiffeCELPathVar = "path"

	// spiffeCELCostLimit bounds one evaluation of an expression an operator
	// wrote. A few split() calls plus indexing and comparisons cost a few dozen
	// units; 10,000 leaves room for an elaborate path convention while still
	// refusing one that does unbounded work on every SPIFFE request.
	spiffeCELCostLimit = 10_000

	// spiffeCELRequestBudget bounds the CEL work one request can cause across
	// every spiffe entry for its issuer. spiffeCELCostLimit bounds a single
	// expression, but parentsFor runs every entry for the issuer, so without
	// this the worst case per request grows with the length of
	// roleMappings.json: a hundred entries near the limit is a million units
	// on every SPIFFE request, from a file that passes every load check.
	//
	// Ten times the per-expression limit keeps a realistic file far from the
	// line while capping per-request cost independently of the file's length,
	// which the per-expression limit alone cannot do. The path conventions a
	// spiffe mapping exists for cost a few dozen units, so a file needs
	// thousands of them for one issuer to reach 100,000. What trips it is a
	// file of expressions that each sit near the per-expression limit, and
	// ten of those already mean the per-expression limit is being used as a
	// target rather than a ceiling.
	//
	// The budget is checked before each evaluation against the actual cost
	// spent so far, so one request does at most this plus one expression's
	// spiffeCELCostLimit of work.
	spiffeCELRequestBudget = 10 * spiffeCELCostLimit
)

// spiffeCELEnv builds the environment every spiffe expression compiles and
// runs against: the path variable, plus ext.Strings() for split(), which is
// not part of CEL's default environment and which a path convention almost
// always needs.
func spiffeCELEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable(spiffeCELPathVar, cel.StringType),
		ext.Strings(),
	)
}

// compileSPIFFEExpression compiles expression, refuses anything that does not
// type-check to bool, and builds the cost-bounded program. It runs once per
// entry, at load; the program is kept on the loaded bundle and then the Local,
// so request time never compiles.
func compileSPIFFEExpression(env *cel.Env, expression string) (cel.Program, error) {
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	if ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("must evaluate to bool, got %s", ast.OutputType())
	}
	return env.Program(ast, cel.CostLimit(spiffeCELCostLimit))
}

// evalSPIFFEExpression runs one compiled expression against path and reports
// the actual cost the evaluation spent, which parentsFor charges against
// spiffeCELRequestBudget. An evaluation error or a cost-limit trip is returned
// with false, and the entry does not match; the caller logs it. A non-bool
// result is false too, and cannot happen: the load-time type check already
// guarantees bool, so the assertion here is defense in depth. It never
// panics, denies or grants on its own.
//
// The cost is cel-go's tracked actual cost, which cel.CostLimit already turns
// on, so reading it adds no work. A program built without cost tracking
// reports no cost, and is charged the full spiffeCELCostLimit.
func evalSPIFFEExpression(program cel.Program, path string) (bool, uint64, error) {
	out, details, err := program.Eval(map[string]any{spiffeCELPathVar: path})
	cost := uint64(spiffeCELCostLimit)
	if actual := details.ActualCost(); actual != nil {
		cost = *actual
	}
	if err != nil {
		return false, cost, err
	}
	matched, ok := out.Value().(bool)
	return ok && matched, cost, nil
}
