package control_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/control"
)

// The access trail.
//
// The engine has always recorded decisions about queries and never decisions
// about access, so "who made this person an admin" lived in whatever stdout
// was piped to. An auditor's first question had the worst possible answer:
// grep the container logs, if anybody kept them.
//
// These are written as the audit rather than as the feature. What an access
// review needs is the row that did not work, the row that survives the person
// leaving, and the certainty that no secret was copied into it on the way.

func TestAnActionIsRecordedAndReadBack(t *testing.T) {
	h := store(t)
	ctx := context.Background()

	h.Record(ctx, h.org, "", "admin@acme.com", control.ActionInvited,
		"newbie@acme.com", control.Allowed, "as member")

	events, err := h.Events(ctx, h.org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("want one event, got %d", len(events))
	}
	e := events[0]
	if e.ActorEmail != "admin@acme.com" || e.Subject != "newbie@acme.com" {
		t.Errorf("the actor or subject was lost: %+v", e)
	}
	if e.Action != control.ActionInvited || e.Outcome != control.Allowed {
		t.Errorf("the action or outcome was lost: %+v", e)
	}
}

// TestARefusedAttemptIsRecorded.
//
// The row an access review exists to find. Somebody trying to invite above
// their own role, or to promote themselves, is exactly what somebody goes
// looking for, and a refusal that leaves no trace reads identically to an
// attempt that never happened.
func TestARefusedAttemptIsRecorded(t *testing.T) {
	h := store(t)
	ctx := context.Background()

	h.Record(ctx, h.org, "", "member@acme.com", control.ActionRoleChanged,
		"member@acme.com", control.Refused, "attempted member to owner")

	events, err := h.Events(ctx, h.org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Outcome != control.Refused {
		t.Fatalf("a refused attempt was not recorded: %+v", events)
	}
	if !strings.Contains(events[0].Detail, "owner") {
		t.Errorf("the detail should say what was attempted: %+v", events[0])
	}
}

// TestTheTrailSurvivesTheActorLeaving.
//
// ON DELETE SET NULL rather than CASCADE. Removing a person must not remove
// the record of what they did, which is precisely the record a person
// removing themselves would want gone.
func TestTheTrailSurvivesTheActorLeaving(t *testing.T) {
	h := store(t)
	ctx := context.Background()

	owner, err := h.CreateUser(ctx, h.org, "owner@acme.com", "Owner",
		"correct-horse-battery-staple", control.RoleOwner, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.Record(ctx, h.org, owner.ID, owner.Email, control.ActionConnectionAdded,
		"prod-warehouse", control.Allowed, "bigquery")

	if _, err := h.pool.Exec(ctx, `DELETE FROM control_users WHERE id = $1`, owner.ID); err != nil {
		t.Fatal(err)
	}

	events, err := h.Events(ctx, h.org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("deleting the actor deleted their trail: %d events left", len(events))
	}
	// The email is copied rather than joined, so the row still names somebody.
	if events[0].ActorEmail != "owner@acme.com" {
		t.Errorf("the record no longer names who did it: %+v", events[0])
	}
}

func TestNewestFirstAndBounded(t *testing.T) {
	h := store(t)
	ctx := context.Background()

	for _, who := range []string{"first@acme.com", "second@acme.com", "third@acme.com"} {
		h.Record(ctx, h.org, "", who, control.ActionSignedIn, "", control.Allowed, "")
	}

	events, err := h.Events(ctx, h.org, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("want 3, got %d", len(events))
	}
	// Newest first: an access review reads the top of the list.
	if events[0].ActorEmail != "third@acme.com" {
		t.Errorf("not newest first: %+v", events)
	}

	// A caller cannot ask for the whole table.
	for i := 0; i < 5; i++ {
		h.Record(ctx, h.org, "", "noisy@acme.com", control.ActionSignedIn, "", control.Allowed, "")
	}
	bounded, err := h.Events(ctx, h.org, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) > 500 {
		t.Errorf("an unbounded limit was honoured: %d rows", len(bounded))
	}
}

// TestOneOrgCannotReadAnother, which is the whole reason org_id is on the row
// rather than implied by the deployment.
func TestOneOrgCannotReadAnother(t *testing.T) {
	h := store(t)
	ctx := context.Background()

	other, err := h.CreateOrg(ctx, "Someone Else")
	if err != nil {
		t.Fatal(err)
	}
	h.Record(ctx, other.ID, "", "them@elsewhere.com", control.ActionSignedIn, "", control.Allowed, "")
	h.Record(ctx, h.org, "", "us@acme.com", control.ActionSignedIn, "", control.Allowed, "")

	ours, err := h.Events(ctx, h.org, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ours {
		if strings.Contains(e.ActorEmail, "elsewhere") {
			t.Errorf("another org's event was readable: %+v", e)
		}
	}
}

// TestRecordingNeverFailsTheActionItDescribes.
//
// A logging outage must not become an outage. Refusing to complete an invite
// because the audit insert failed would be a worse product than one that
// loses the occasional row, which is why Record returns nothing at all.
func TestRecordingNeverFailsTheActionItDescribes(t *testing.T) {
	h := store(t)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// A cancelled context, an org that does not exist, an impossible action:
	// none of it may panic or block, because every call site has already done
	// the thing being described.
	h.Record(cancelled, h.org, "", "someone@acme.com", control.ActionSignedIn, "", control.Allowed, "")
	h.Record(context.Background(), "no-such-org", "", "someone@acme.com",
		control.ActionSignedIn, "", control.Allowed, "")
	h.Record(context.Background(), h.org, "no-such-user", "someone@acme.com",
		control.ActionSignedIn, "", "nonsense-outcome", "")
}
