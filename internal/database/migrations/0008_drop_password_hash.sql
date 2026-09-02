-- Remove password authentication from the schema.
--
-- /auth/register and /auth/login are gone: nothing shipped used them — the iOS client
-- signs in with Apple and renews at /auth/refresh — while they were a live,
-- unauthenticated way for anyone to create an account at any address. That is what made
-- the pre-hijack in docs/AUDIT-3.md C1 possible, and what obliged /auth/register to
-- disclose which addresses are taken (L5). With the endpoints gone, password_hash is a
-- column nothing reads and nothing can write, holding the only credential material this
-- database stores.
--
-- The guard below is the point of doing this as a migration rather than a hand-run
-- statement. Dropping the column is what strands an account that has a password and no
-- Apple identity: nothing else can authenticate it, no endpoint can attach an Apple
-- identity to it (linking requires a session, and a session requires signing in), and
-- there is no password reset. Such an account's data would still be there and be
-- unreachable forever.
--
-- So the migration refuses instead, and because migrations run at boot, a deploy that
-- would strand somebody fails loudly with their count rather than succeeding quietly.
-- Every account created by Sign in with Apple has an apple_sub, so the expected answer
-- is zero and this costs nothing; a non-zero answer is a real question that has to be
-- settled by a person, by linking those accounts to their Apple subject:
--
--   UPDATE user_accounts SET apple_sub = '<the coach''s Apple subject>' WHERE id = '<id>';
--
-- A local database seeded before this change holds exactly one such row (the old
-- coach@soccerkit.dev / password123 account). Clear it and re-run `make seed`, which now
-- plants an Apple identity instead.
--
-- The check and the drop have to arrive together, and they do because database.Migrate
-- applies each file inside one transaction: the RAISE aborts it and the ALTER never
-- commits (verified — the refusal leaves the column in place). Run this file through a
-- tool that executes statements independently, `psql -f` among them, and the guard
-- fails open: the DO block errors and the ALTER then runs anyway. Use `psql -1 -f` if
-- you ever apply it by hand.

DO $$
DECLARE stranded bigint;
BEGIN
    SELECT count(*) INTO stranded
    FROM user_accounts
    WHERE password_hash IS NOT NULL AND apple_sub IS NULL;

    IF stranded > 0 THEN
        RAISE EXCEPTION
            'refusing to drop user_accounts.password_hash: % account(s) have a password '
            'and no Apple identity, and dropping it would lock them out permanently — '
            'there is no password reset and no way to link an Apple identity without '
            'first signing in. Give each one its apple_sub, or delete it, then migrate.',
            stranded;
    END IF;
END $$;

ALTER TABLE user_accounts DROP COLUMN password_hash;
