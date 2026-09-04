-- Device tokens, so a push has somewhere to go.
--
-- An invitation is the first thing this service has that a person needs to be told
-- about rather than to go looking for: it is addressed to someone who may not be in the
-- app, and until they open it and check /me/invitations they have no idea it exists.
--
-- The token is APNs' handle for one install of the app on one device, which is why it,
-- and not the person, is the primary key. A token identifies a device and moves between
-- people: sign out, hand the phone to a colleague, sign in as them, and Apple issues the
-- same token for the same install. If person_id were the key that second sign-in would
-- add a row and the first person would keep receiving the club's pushes on a phone that
-- is no longer theirs. Keyed on the token, re-registering simply moves it.
CREATE TABLE device_tokens (
    token        text PRIMARY KEY,
    person_id    uuid NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    platform     text NOT NULL DEFAULT 'ios' CHECK (platform IN ('ios')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Refreshed every time the app registers, which it does on launch. A token Apple
    -- has not rejected but that has not been seen for months is a candidate for pruning
    -- long before it becomes a delivery failure.
    last_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_device_tokens_person ON device_tokens (person_id);
