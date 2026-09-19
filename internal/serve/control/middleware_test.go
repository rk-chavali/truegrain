package control

import (
	"slices"
	"testing"

	"github.com/rk-chavali/truegrain/internal/control"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
)

// The identity the governance gate sees.
//
// Written as the attack, like the store's own tests. The role travels to
// the engine as a reserved group, because the engine matches subjects and
// groups and has no concept of a control plane role. That only holds if
// the reservation is real: groups are typed by an admin on the People
// screen, so a group somebody was given must never be able to impersonate
// a role somebody holds.

func TestAGivenGroupCannotImpersonateARole(t *testing.T) {
	// An admin types this into a member's groups. It is a plausible thing
	// to do by accident and an obvious thing to do on purpose.
	member := &control.User{
		Email:  "member@acme.test",
		Role:   control.RoleMember,
		Groups: []string{"analysts", "role:admin", "role:owner"},
	}

	id := identityOf(member)

	for _, forged := range []string{"role:admin", "role:owner"} {
		if slices.Contains(id.Groups, forged) {
			t.Fatalf("a given group survived as %q: groups are %v", forged, id.Groups)
		}
	}
	if !slices.Contains(id.Groups, "role:member") {
		t.Fatalf("the real role is missing: groups are %v", id.Groups)
	}
	// The group they were actually given is still theirs; only the
	// reserved prefix is taken away.
	if !slices.Contains(id.Groups, "analysts") {
		t.Fatalf("a legitimate group was dropped: groups are %v", id.Groups)
	}
}

// The property the stripping exists for, stated end to end rather than as
// a claim about a slice: the console names its audit readers by these
// groups, so a forged one must not open the audit log.
func TestAForgedRoleGroupDoesNotOpenTheAuditLog(t *testing.T) {
	readers := rest.NewAuditReaders([]string{
		"group:" + RoleGroupPrefix + string(control.RoleOwner),
		"group:" + RoleGroupPrefix + string(control.RoleAdmin),
	})

	forged := identityOf(&control.User{
		Email:  "member@acme.test",
		Role:   control.RoleMember,
		Groups: []string{"role:admin"},
	})
	if readers.Allows(forged) {
		t.Fatal("a member with a forged role group could read the audit log")
	}

	real := identityOf(&control.User{
		Email: "admin@acme.test",
		Role:  control.RoleAdmin,
	})
	if !readers.Allows(real) {
		t.Fatal("a real admin could not read the audit log")
	}

	viewer := identityOf(&control.User{
		Email: "viewer@acme.test",
		Role:  control.RoleViewer,
	})
	if readers.Allows(viewer) {
		t.Fatal("a viewer could read the audit log")
	}
}

// An unauthenticated request carries no identity, and an empty identity
// must not match a reader. AuditReaders already refuses an empty subject;
// this pins that the console's side of it produces one.
func TestNoSessionIsNoIdentity(t *testing.T) {
	id := identityOf(nil)
	if id.Subject != "" || len(id.Groups) != 0 {
		t.Fatalf("an absent user produced an identity: %+v", id)
	}

	readers := rest.NewAuditReaders([]string{"group:" + RoleGroupPrefix + "admin"})
	if readers.Allows(id) {
		t.Fatal("an anonymous caller could read the audit log")
	}
}
