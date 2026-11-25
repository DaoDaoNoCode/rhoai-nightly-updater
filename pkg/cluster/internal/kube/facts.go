package kube

import "errors"

// The cluster facts a change reads with the ServiceAccount's token.
//
// Every change and every in-project read runs with the signed-in user's own
// token. A few things a project action needs are cluster-wide and no
// secret: where RHOAI runs its applications (the DSCInitialization),
// whether a component is turned on (the DataScienceCluster), whether the
// control plane is hosted (Infrastructure cluster), the dashboard's and
// MLflow's addresses. A project user's token may read none of them, so the
// change reads them with the ServiceAccount's client, set with SetFacts,
// through Facts: a read-only copy that sends nothing but GET and HEAD and
// gives no token to another server. The ServiceAccount reads them with the
// rules deploy/template.yaml already grants it (the RBAC tests reach every
// function a Facts() client is passed to from the ServiceAccount's scan,
// TestFactsClientOnlyReads).

// ErrFactsReadOnly: a write was sent with a facts client (Facts), which only
// reads. Nothing was sent.
var ErrFactsReadOnly = errors.New("the cluster facts client only reads; a change is made with the signed-in user's own client")

// ErrNoFactsClient: a cluster fact that only the ServiceAccount reads (into
// a cache all users share) was asked of a client that acts with the
// signed-in user's token and has no facts client (SetFacts). Nothing was
// read.
var ErrNoFactsClient = errors.New("this cluster fact is read only with the updater's ServiceAccount, and this client acts with the signed-in user's token without it")

// SetFacts makes f (the ServiceAccount's client) the client Facts reads
// the cluster facts with: pkg/api sets it on a change's client and on a
// user read's, next to SetBookkeeping.
func (c *Client) SetFacts(f *Client) { c.facts = f }

// Facts returns the client of the cluster facts: a copy of the client set
// with SetFacts, else of c itself (a ServiceAccount client of a read or a
// background job reads them with its own token anyway), with c's context,
// user, status-read mark and retry rule. The copy is read-only: it sends
// only GET and HEAD (any other request fails with ErrFactsReadOnly before
// it is sent), its BearerToken is empty, and it carries no bookkeeper,
// signed-in user or facts client of its own.
//
// A client that acts as the user and has no facts client gives a copy that
// still acts as the user: its reads are checked against the user's RBAC,
// and the reads that fill a cache all users share refuse it
// (ErrNoFactsClient, rhoai.DiscoverNamespaces).
func (c *Client) Facts() *Client {
	f := *c.factsSource()
	f.ctx, f.statusRead, f.noRetry = c.ctx, c.statusRead, c.noRetry
	f.bookkeeper, f.signedInUser, f.facts = nil, nil, nil
	f.readOnly = true
	if f.username == "" {
		f.username = c.username
	}
	return &f
}

// factsSource is the client Facts copies: the one SetFacts set, else c.
func (c *Client) factsSource() *Client {
	if c.facts != nil {
		return c.facts
	}
	return c
}

// ReadOnly reports whether the client only reads (a facts client, Facts).
func (c *Client) ReadOnly() bool { return c.readOnly }
