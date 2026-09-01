-- Store refresh tokens as hashes, not plaintext.
--
-- refresh_tokens.token held the token verbatim and was looked up by that value, so
-- anyone with read access to the database — a backup, a log, an over-broad support
-- query — held working credentials for every account. The token is a 48-byte random
-- string, so a plain SHA-256 is enough: there is no low-entropy guess to grind, and a
-- slow KDF would only add latency to every refresh.
--
-- Existing rows cannot be migrated (the plaintext is the only thing we have, and
-- hashing it in place would keep it usable by anyone who already copied it), so they
-- are deleted. Everyone signs in again once; the access tokens they already hold keep
-- working until they expire.

DELETE FROM refresh_tokens;

ALTER TABLE refresh_tokens RENAME COLUMN token TO token_hash;
