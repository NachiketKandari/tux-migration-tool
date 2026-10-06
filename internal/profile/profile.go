// Package profile is the target-profile seam (PRD-2026-09-10
// architecture-first Part II, AD10–AD14): a project's conventions — naming
// policy, layer layout, DB rules, template set — are profile data, not
// pipeline code. "Any template" is the founding requirement: one Go
// project's db/controller/handler shape is a template, another Go project's
// is a different one, the Python batch is a template plus rules, and .NET
// Core lands as a third profile later (P4/P5, its own PRD).
//
// P0 skeleton: the TargetProfile interface and the registry only — nothing
// consumes it yet (P1 extracts goitem). The interface is deliberately small:
// knobs the A8.3 extraction list proved are single-homed; anything that
// refuses to move cleanly stays goitem-invariant in the profile contract
// rather than forced (P1 review rule, AD14).
package profile

// Profile is one target's conversion conventions. Implementations are
// data-shaped (values over interfaces' behavior) so a new project's
// conventions are config + template assets, not a fork of the pipeline.
// (The Layout surface — layer folder names, service dir — was deleted as
// dead API: no engine read it; the tree shape lives in the plan emitter.
// engine-wiring audit Tier-2.)
type Profile interface {
	// ID is the config selector ("target.profile"; absent config = goitem).
	ID() string
	// Naming resolves the profile's identifier policies: request/response
	// struct names for an endpoint, the row struct name for a query
	// (fallback naming; mapping pins win before this is consulted).
	Naming() Naming
	// DB resolves the data-access rules: tx-variant policy and the store
	// receiver the prompts/gates reference.
	DB() DBRules
}

// Naming carries the identifier policies (the AD3 named variants — a
// profile picks one policy per shape; the policies themselves are shared,
// documented functions in internal/common).
type Naming struct {
	// RequestType renders the endpoint's request struct name ("ItemList" →
	// "ItemListRequest").
	Request func(endpoint string) string
	// ResponseType renders the endpoint's response struct name.
	Response func(endpoint string) string
	// RowName renders the row struct for one query's results (verb-stripped
	// fallback naming; "MergeDemoAccounts" → "DemoAccounts").
	Row func(methodName string) string
	// Receiver renders the lower-cased controller/handler receiver type
	// ("Item" → "item").
	Receiver func(service string) string
}

// DBRules carries the data-access conventions the deterministic generators
// and the controller prompts/gates share.
type DBRules struct {
	// StoreReceiver is the receiver prefix in prompts and the required-call
	// gate ("s.store.GetItemDetails(...)").
	StoreReceiver string
	// TxVariants marks the DML types that render through tx-variant
	// templates (`tx *sqlx.Tx` second parameter, decision 27); MERGE keeps
	// its DML contract template (F2).
	TxVariants func(queryType string) bool
}
