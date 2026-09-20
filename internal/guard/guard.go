// Package guard inspects tool calls before they run.
package guard

import (
	"encoding/json"
	"time"
)

// Guards can only deny, and that is the entire design.
//
// The obvious interface returns allow, deny, or no-opinion. It is also the
// source of an entire class of bug: once a guard can allow, the outcome
// depends on the order guards were registered in, and every future guard has
// to be reasoned about against every existing one. "Is this call permitted"
// stops having a single answer and starts having an answer per ordering.
//
// Deny-only makes the chain monotonic. A denial is final, no later guard can
// reverse it, and adding a guard can only ever make the system more
// restrictive — never less. Ordering then affects which *message* the model
// sees, and nothing else. Quoting the DeepSeek Harness source this follows:
// "Because guards have no allow result, listener ordering cannot turn a
// denial back into permission."
//
// One consequence worth stating: there is deliberately no override. A call
// that must be permitted is permitted by not writing a guard that denies it,
// not by writing a second guard that argues.

// Call is a tool call that already happened in this request.
type Call struct {
	Tool    string
	Args    string
	IsError bool
	Code    string
}

// Execution is a pending tool call, as a guard sees it.
type Execution struct {
	Tool      string
	CallID    string
	Args      json.RawMessage
	Turn      int
	SessionID string

	// Remaining is the wall clock left in the request's budget, or zero if
	// there is no wall-clock limit.
	Remaining time.Duration

	// Prior is every tool call already made in this request, oldest first.
	// This is what makes loop detection possible: a single call in isolation
	// never looks wrong.
	Prior []Call
}

// Guard inspects a pending call. An empty return means no opinion; anything
// else denies the call, and the text goes to the model as the tool result.
//
// That makes the wording part of the contract rather than decoration. A
// denial the model cannot act on produces an immediate identical retry.
type Guard interface {
	Name() string
	Check(Execution) string
}

// Func adapts a function to Guard.
type Func struct {
	name string
	fn   func(Execution) string
}

// New builds a named guard from a function.
func New(name string, fn func(Execution) string) Guard {
	return &Func{name: name, fn: fn}
}

func (f *Func) Name() string              { return f.name }
func (f *Func) Check(ex Execution) string { return f.fn(ex) }

// Chain is an ordered set of guards. The zero value is usable and denies
// nothing.
type Chain struct {
	guards []Guard
}

// NewChain builds a chain.
func NewChain(gs ...Guard) *Chain {
	return &Chain{guards: gs}
}

// Add appends a guard. Later guards cannot undo an earlier denial, so the only
// thing position affects is which message the model sees when several guards
// would object.
func (c *Chain) Add(g Guard) {
	if g != nil {
		c.guards = append(c.guards, g)
	}
}

// Len reports how many guards are installed.
func (c *Chain) Len() int {
	if c == nil {
		return 0
	}
	return len(c.guards)
}

// Names lists the installed guards, in order.
func (c *Chain) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.guards))
	for _, g := range c.guards {
		out = append(out, g.Name())
	}
	return out
}

// Check runs the chain and reports the first denial, with the name of the
// guard that made it. A nil chain permits everything.
func (c *Chain) Check(ex Execution) (reason, by string) {
	if c == nil {
		return "", ""
	}
	for _, g := range c.guards {
		if r := g.Check(ex); r != "" {
			return r, g.Name()
		}
	}
	return "", ""
}
