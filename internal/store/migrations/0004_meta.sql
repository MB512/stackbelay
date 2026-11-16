-- Small key/value settings of the database itself. "key_check" holds a
-- known plaintext encrypted with the secret key, so a wrong key is
-- detected at startup instead of on the first repository access.
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL
) STRICT;
