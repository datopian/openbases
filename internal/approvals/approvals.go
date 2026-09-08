// Package approvals implements the approval state machine (WP-F2).
//
// Every approval is bound to an action digest. That is the whole design: what
// is approved is a specific action, not an intention. If the diff, plan or
// parameters change after approval, the approval no longer applies — otherwise
// "approved" degrades into "someone once said yes to something like this",
// which is exactly the failure a protected-action gate exists to prevent
// (ADR-0009).
package approvals

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/datopian/openbases/internal/authz"
)

// Refusal is a structured refusal.
//
// A bare "forbidden" tells the caller nothing about what to do next. Naming the
// policy, who could act, and the remediation turns a dead end into an
// instruction — and makes the rule legible in the interface rather than only in
// a database trigger.
type Refusal struct {
	Code         string `json:"code"`
	Policy       string `json:"policy"`
	Message      string `json:"message"`
	AllowedActor string `json:"allowed_actor,omitempty"`
	Remediation  string `json:"remediation,omitempty"`
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// Decision is a single approver's verdict.
type Decision struct {
	RequestID string
	UserID    string
	Approve   bool
	Reason    string
	// Digest the approver actually saw. Compared against the request's current
	// digest so approving a stale view of the action is caught rather than
	// silently accepted.
	SeenDigest string
}

// State is the answer to "may this action run now".
type State struct {
	Status            string `json:"status"`
	ApprovalsRecorded int    `json:"approvals_recorded"`
	ApprovalsRequired int    `json:"approvals_required"`
	Digest            string `json:"action_digest"`
	Unlocked          bool   `json:"unlocked"`
	Reason            string `json:"reason,omitempty"`
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Digest canonicalises an action into the value that gets approved.
//
// Callers must not invent their own: two callers hashing the same action
// differently would produce approvals that cannot be matched, and the mismatch
// would look like tampering.
func Digest(actionType string, parameters []byte) string {
	h := sha256.New()
	h.Write([]byte(actionType))
	h.Write([]byte{0}) // separator, so ("ab","c") and ("a","bc") differ
	h.Write(parameters)
	return hex.EncodeToString(h.Sum(nil))
}

// Decide records one approver's decision.
//
// Refusals are returned as *Refusal so the API can surface the policy that
// stopped the caller.
func (s *Store) Decide(ctx context.Context, d Decision) error {
	if d.RequestID == "" || d.UserID == "" {
		return errors.New("a decision needs a request and a decider")
	}
	if !d.Approve && d.Reason == "" {
		return &Refusal{
			Code:        "reason_required",
			Policy:      "reject_requires_reason",
			Message:     "a rejection must say why",
			Remediation: "Supply a reason describing what would need to change.",
		}
	}

	return authz.WithUser(ctx, s.db, d.UserID, func(tx *sql.Tx) error {
		var status, digest, requester string
		var expires time.Time
		var required int
		err := tx.QueryRowContext(ctx, `
			SELECT status, action_digest, required_approvals, expires_at,
			       COALESCE(requested_by_user_id::text, '')
			  FROM approval_requests WHERE id = $1::uuid`, d.RequestID).
			Scan(&status, &digest, &required, &expires, &requester)
		if errors.Is(err, sql.ErrNoRows) {
			// Same answer whether it does not exist or is invisible to this
			// caller. Distinguishing them confirms a restricted action exists.
			return &Refusal{
				Code:    "not_found",
				Policy:  "row_level_security",
				Message: "no such approval request",
			}
		}
		if err != nil {
			return err
		}

		if status != "pending" {
			return &Refusal{
				Code:        "not_pending",
				Policy:      "single_decision_window",
				Message:     fmt.Sprintf("this request is %s and can no longer be decided", status),
				Remediation: "Raise a new request if the action still needs to happen.",
			}
		}
		if time.Now().After(expires) {
			return &Refusal{
				Code:        "expired",
				Policy:      "approval_expiry",
				Message:     "this request has expired",
				Remediation: "Raise a new request; an approval that outlives its window is not evidence of current intent.",
			}
		}

		// The digest the approver saw must be the digest on the request. This
		// catches the race where the action changes between rendering the page
		// and pressing approve — the approver would otherwise be recorded as
		// having approved something they never saw.
		if d.SeenDigest != "" && d.SeenDigest != digest {
			return &Refusal{
				Code:        "digest_changed",
				Policy:      "action_digest_binding",
				Message:     "the action changed after it was shown to you",
				Remediation: "Reload the request and review the current action before deciding.",
			}
		}

		if requester != "" && requester == d.UserID {
			return &Refusal{
				Code:         "self_approval",
				Policy:       "not_creator",
				Message:      "the person who requested an action may not approve it",
				AllowedActor: "any other holder of a required role",
				Remediation:  "Ask a colleague with the required role to review it.",
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO approval_decisions
			    (request_id, decided_by_user_id, decision, reason, decided_digest)
			VALUES ($1::uuid, $2::uuid, $3, NULLIF($4,''), $5)`,
			d.RequestID, d.UserID, map[bool]string{true: "approve", false: "reject"}[d.Approve],
			d.Reason, digest); err != nil {
			return err
		}

		// A rejection ends it immediately. Approvals only close the request
		// once enough have been recorded.
		if !d.Approve {
			_, err := tx.ExecContext(ctx,
				`UPDATE approval_requests SET status = 'rejected' WHERE id = $1::uuid`, d.RequestID)
			return err
		}

		_, err = tx.ExecContext(ctx, `
			UPDATE approval_requests SET status = 'approved'
			 WHERE id = $1::uuid
			   AND (SELECT count(*) FROM approval_decisions
			         WHERE request_id = $1::uuid AND decision = 'approve') >= required_approvals`,
			d.RequestID)
		return err
	})
}

// StateFor answers whether an action may run, against the digest of the action
// about to be executed.
//
// currentDigest is passed in rather than read from the request, because the
// question is not "was this approved" but "is what I am about to do the thing
// that was approved".
func (s *Store) StateFor(ctx context.Context, userID, requestID, currentDigest string) (State, error) {
	var st State

	err := authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		var expires time.Time
		err := tx.QueryRowContext(ctx, `
			SELECT r.status, r.action_digest, r.required_approvals, r.expires_at,
			       (SELECT count(*) FROM approval_decisions d
			         WHERE d.request_id = r.id AND d.decision = 'approve')
			  FROM approval_requests r WHERE r.id = $1::uuid`, requestID).
			Scan(&st.Status, &st.Digest, &st.ApprovalsRequired, &expires, &st.ApprovalsRecorded)
		if errors.Is(err, sql.ErrNoRows) {
			return &Refusal{Code: "not_found", Policy: "row_level_security", Message: "no such approval request"}
		}
		if err != nil {
			return err
		}

		switch {
		case st.Status == "rejected":
			st.Reason = "the request was rejected"
		case st.Status == "invalidated":
			st.Reason = "the action changed after approval"
		case time.Now().After(expires):
			st.Reason = "the approval window has expired"
		case st.ApprovalsRecorded < st.ApprovalsRequired:
			st.Reason = fmt.Sprintf("%d of %d approvals recorded",
				st.ApprovalsRecorded, st.ApprovalsRequired)
		case currentDigest != "" && currentDigest != st.Digest:
			// The action changed. This is the case the whole design exists for.
			st.Reason = "the action about to run is not the action that was approved"
		default:
			st.Unlocked = st.Status == "approved"
			if !st.Unlocked {
				st.Reason = "not yet approved"
			}
		}
		return nil
	})
	return st, err
}

// Invalidate marks a request as no longer applicable because its action changed.
//
// Separate from rejection: nobody decided against it, the thing it described
// stopped existing. Keeping them distinct matters for the audit trail, because
// "rejected" is a judgement about a person's proposal and "invalidated" is not.
func (s *Store) Invalidate(ctx context.Context, userID, requestID, newDigest string) error {
	return authz.WithUser(ctx, s.db, userID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE approval_requests
			   SET status = 'invalidated'
			 WHERE id = $1::uuid
			   AND status IN ('pending', 'approved')
			   AND action_digest <> $2`,
			requestID, newDigest)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return &Refusal{
				Code:    "not_invalidated",
				Policy:  "action_digest_binding",
				Message: "the request was already closed, or its digest is unchanged",
			}
		}
		return nil
	})
}
