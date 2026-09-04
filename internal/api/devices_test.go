package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const deviceToken = "aa11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00112233"

// TestDeviceRegistrationIsIdempotent — the app registers on every launch and Apple
// reissues the same token for the same install, so a repeat is the normal case.
func TestDeviceRegistrationIsIdempotent(t *testing.T) {
	resetDB(t)
	coach, coachID := signInCoach(t, "dev-reg@e.com")

	for i := 0; i < 2; i++ {
		r := do(t, http.MethodPost, "/api/v1/me/devices", coach,
			map[string]any{"token": deviceToken, "platform": "ios"})
		if r.status != http.StatusOK {
			t.Fatalf("register #%d: %d %s", i+1, r.status, r.raw)
		}
	}
	var count int
	var owner string
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*), max(person_id::text) FROM device_tokens WHERE token = $1`,
		deviceToken).Scan(&count, &owner); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("registering twice should leave one row, got %d", count)
	}
	if owner != coachID {
		t.Errorf("token belongs to %s, want %s", owner, coachID)
	}
}

// TestDeviceTokenFollowsWhoeverSignedInLast is why the token is the primary key rather
// than the person. Hand the phone over, sign in as someone else, and Apple issues the
// same token -- if the row did not move, the previous owner would go on receiving pushes
// on a device that is no longer theirs.
func TestDeviceTokenFollowsWhoeverSignedInLast(t *testing.T) {
	resetDB(t)
	first, _ := signInCoach(t, "dev-first@e.com")
	second, secondID := signInCoach(t, "dev-second@e.com")

	if r := do(t, http.MethodPost, "/api/v1/me/devices", first,
		map[string]any{"token": deviceToken}); r.status != http.StatusOK {
		t.Fatalf("first register: %d %s", r.status, r.raw)
	}
	if r := do(t, http.MethodPost, "/api/v1/me/devices", second,
		map[string]any{"token": deviceToken}); r.status != http.StatusOK {
		t.Fatalf("second register: %d %s", r.status, r.raw)
	}

	var count int
	var owner string
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*), max(person_id::text) FROM device_tokens WHERE token = $1`,
		deviceToken).Scan(&count, &owner); err != nil {
		t.Fatal(err)
	}
	if count != 1 || owner != secondID {
		t.Errorf("the token should have moved to the new signer; count=%d owner=%s want=%s",
			count, owner, secondID)
	}
}

// TestUnregisteringIsScopedToTheOwner — holding someone else's token must not be enough
// to silence their phone.
func TestUnregisteringIsScopedToTheOwner(t *testing.T) {
	resetDB(t)
	owner, _ := signInCoach(t, "dev-owner@e.com")
	stranger, _ := signInCoach(t, "dev-stranger@e.com")

	do(t, http.MethodPost, "/api/v1/me/devices", owner, map[string]any{"token": deviceToken})

	if r := do(t, http.MethodDelete, "/api/v1/me/devices/"+deviceToken, stranger, nil); r.status != http.StatusOK {
		t.Fatalf("unregister answers ok either way: %d %s", r.status, r.raw)
	}
	var count int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM device_tokens WHERE token = $1`, deviceToken).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("a stranger removed someone else's device")
	}

	// The owner can, and doing it twice is not an error.
	for i := 0; i < 2; i++ {
		if r := do(t, http.MethodDelete, "/api/v1/me/devices/"+deviceToken, owner, nil); r.status != http.StatusOK {
			t.Fatalf("owner unregister #%d: %d %s", i+1, r.status, r.raw)
		}
	}
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM device_tokens WHERE token = $1`, deviceToken).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("the owner's unregister did not take")
	}
}

// TestDeviceTokenShapeIsChecked — the token becomes a path segment in the request to
// Apple, and a malformed one is a client bug worth reporting now rather than a delivery
// failure logged later.
func TestDeviceTokenShapeIsChecked(t *testing.T) {
	resetDB(t)
	coach, _ := signInCoach(t, "dev-shape@e.com")
	for _, bad := range []string{"", "not-hex", strings.Repeat("a", 63), strings.Repeat("z", 64)} {
		r := do(t, http.MethodPost, "/api/v1/me/devices", coach, map[string]any{"token": bad})
		if r.status != http.StatusBadRequest {
			t.Errorf("token %q should be rejected, got %d %s", bad, r.status, r.raw)
		}
	}
}

// TestInvitingNotifiesTheInvitee is the point of the whole feature: an invitation is
// addressed to someone who may not have the app open, and until now nothing told them.
func TestInvitingNotifiesTheInvitee(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "note-admin@e.com")
	orgID := orgOf(t, admin)
	_, inviteeID := signInCoach(t, "note-invitee@e.com")

	// Give the club a name worth recognising on a lock screen.
	do(t, http.MethodPatch, "/api/v1/organizations/"+orgID, admin, map[string]any{"name": "Riverside FC"})
	testNotes.drain()

	r := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
		map[string]any{"email": "note-invitee@e.com", "roles": []string{"coach"}})
	if r.status != http.StatusCreated {
		t.Fatalf("invite: %d %s", r.status, r.raw)
	}

	notes := testNotes.drain()
	if len(notes) != 1 {
		t.Fatalf("expected one notification, got %d", len(notes))
	}
	got := notes[0]
	if got.personID.String() != inviteeID {
		t.Errorf("notified %s, want the invitee %s", got.personID, inviteeID)
	}
	if !strings.Contains(got.note.Title, "Riverside FC") {
		t.Errorf("the title should name the club, got %q", got.note.Title)
	}
	if !strings.Contains(got.note.Body, "coach") {
		t.Errorf("the body should say what is being offered, got %q", got.note.Body)
	}
	if got.note.Data["invitationId"] != r.body["id"].(string) {
		t.Errorf("the payload should carry the invitation id so a tap can open it: %v", got.note.Data)
	}
	// The inviter's name is a third party's information on a device that has not
	// accepted anything yet.
	if strings.Contains(got.note.Body, "note-admin") {
		t.Errorf("the body should not carry the inviter's identity: %q", got.note.Body)
	}
}

// TestInvitingAnAddressWithNoAccountNotifiesNobody — there is nobody to tell yet, and
// that is the waiting case working rather than a failure.
func TestInvitingAnAddressWithNoAccountNotifiesNobody(t *testing.T) {
	resetDB(t)
	admin, _ := signInCoach(t, "note-none@e.com")
	orgID := orgOf(t, admin)
	testNotes.drain()

	if r := do(t, http.MethodPost, "/api/v1/organizations/"+orgID+"/invitations", admin,
		map[string]any{"email": "stranger-with-no-account@e.com", "roles": []string{"coach"}}); r.status != http.StatusCreated {
		t.Fatalf("invite: %d %s", r.status, r.raw)
	}
	if notes := testNotes.drain(); len(notes) != 0 {
		t.Errorf("expected no notification, got %d", len(notes))
	}
}
