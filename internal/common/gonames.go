package common

// Generated-Go naming convention: the identifiers every Go emitter and every
// Go validation gate agrees on.
//
// These live here rather than as literals at each site because the same names
// are load-bearing in two independent directions. The emitters write them into
// the rendered code, and the gates in internal/convert *parse the rendered
// code expecting them* — an identifier allow-list, a helper-call regexp, a
// fixed-signature substring match, and the synthetic body wrapper used for the
// compile check. A gate that disagrees with the template it validates does not
// fail loudly: the regexp simply stops matching and the orchestration contract
// goes unenforced, or every body is rejected as an undefined identifier. One
// definition, referenced by both directions, is what keeps those two sides
// from drifting.
//
// The convention is layer-role naming — s for store, c for controller, h for
// handler — with ctx for the context on every layer, so a reader can tell the
// layer from the receiver alone and no identifier is ever doing double duty as
// both a receiver and a context.
const (
	// GoStoreRecv is the receiver on generated store (db) methods:
	// func (s *store) GetItem(ctx context.Context, ...) — s for store.
	GoStoreRecv = "s"

	// GoCtrlRecv is the receiver on generated controller methods:
	// func (c *itemController) ItemList(ctx context.Context, ...) — c for
	// controller. It reaches the store through the c.store field.
	GoCtrlRecv = "c"

	// GoHandlerRecv is the receiver on generated gin handler methods:
	// func (h *itemHandler) ItemList(ctx *gin.Context) — h for handler.
	GoHandlerRecv = "h"

	// GoCtx is the context parameter name on every generated layer — the
	// context.Context on store and controller methods, the *gin.Context on
	// handler methods. Freed by the old single-letter "c", which had to
	// double as the controller receiver.
	GoCtx = "ctx"

	// GoStoreField is the controller struct field holding the store
	// interface, so a controller method calls c.store.GetItem(ctx, ...).
	GoStoreField = "store"

	// GoTx is the *sqlx.Tx handle parameter on tx-variant methods and the
	// name bound in ExecTransaction closures.
	GoTx = "tx"
)

// GoStoreCallPrefix is the receiver path a controller method uses to reach the
// store — "c.store." — as consumed by the prompt contract lines and the
// required-store-call gate.
func GoStoreCallPrefix() string {
	return GoCtrlRecv + "." + GoStoreField + "."
}

// goReservedParams are the identifiers a generated method already spends: its
// receiver, its context, its transaction handle, and the locals the templates
// bind in the body. A derived parameter that would land on one of them is a
// redeclaration the compiler rejects, and several gates treat an unknown
// identifier as a hard failure, so the collision is caught at naming time
// instead.
//
// Matching is exact-token, never prefix: the corpus is full of binds like
// c_mtch_accnt and c_user_id, which derive to cMtchAccnt/cUserId and are
// perfectly legal beside a ctx parameter.
var goReservedParams = map[string]bool{
	GoStoreRecv:   true,
	GoCtrlRecv:    true,
	GoHandlerRecv: true,
	GoCtx:         true,
	GoTx:          true,
	// the templates' own body locals and the method's named returns
	"request":  true,
	"response": true,
	"data":     true,
	"err":      true,
	"query":    true,
	"result":   true,
}

// GoParamReserved reports whether name is already spent by a generated method's
// own signature — receiver, context, tx handle, or a named return / local the
// template body binds. Exact-token only: c_mtch_accnt → cMtchAccnt is legal
// beside a ctx parameter.
func GoParamReserved(name string) bool {
	return goReservedParams[name]
}
