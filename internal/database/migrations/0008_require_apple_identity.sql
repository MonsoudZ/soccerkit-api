-- Refuse to run a build that has removed password authentication while any account
-- still depends on it.
--
-- /auth/register and /auth/login are gone. An account holding a password and no Apple
-- identity can no longer authenticate by any means: nothing else identifies it, no
-- endpoint attaches an Apple identity to it (linking requires a session, and a session
-- requires signing in), and there is no password reset. Its data would still be there
-- and be unreachable forever.
--
-- This check ships with the code that removes those endpoints, not with the migration
-- that drops the column, because removing the endpoints is what does the locking out.
-- The column is dropped a release later (0009), and by then this has already run.
--
-- Migrations run at boot, so a deploy that would strand somebody fails with their count
-- rather than succeeding quietly. Every account created by Sign in with Apple has an
-- apple_sub, so the expected answer is zero and this costs nothing. A non-zero answer is
-- a question for a person, settled by giving each account its Apple subject:
--
--   UPDATE user_accounts SET apple_sub = '<the coach''s Apple subject>' WHERE id = '<id>';
--
-- A local database seeded before this change holds exactly one such row (the old
-- coach@soccerkit.dev / password123 account). Clear it and re-run `make seed`, which now
-- plants an Apple identity instead.
--
-- The column-existence guard is for a database that already ran an earlier build of this
-- migration set, where 0008 dropped the column outright; there the question is already
-- settled and there is nothing left to check.

DO $$
DECLARE stranded bigint;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'user_accounts'
          AND column_name = 'password_hash'
    ) THEN
        RETURN;
    END IF;

    EXECUTE 'SELECT count(*) FROM user_accounts WHERE password_hash IS NOT NULL AND apple_sub IS NULL'
        INTO stranded;

    IF stranded > 0 THEN
        RAISE EXCEPTION
            'refusing to remove password authentication: % account(s) have a password '
            'and no Apple identity, and this build has no way for them to sign in — '
            'there is no password login, no reset, and no way to link an Apple identity '
            'without first signing in. Give each one its apple_sub, or delete it, then '
            'deploy.',
            stranded;
    END IF;
END $$;
