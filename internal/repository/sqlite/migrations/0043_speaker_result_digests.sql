-- 0043_speaker_result_digests: opaque result digests make biometric staging
-- and commits idempotent and mutually exclusive.
--
-- remote.DigestResult hashes the canonical wire form of an inference result.
-- The digest is a one-way identity for the result bytes, never their content:
-- a vector still never enters SQLite, a log line or an API response. A repeat
-- submission of the same bytes after a lost HTTP response is answered from the
-- recorded digest instead of staging a second vector, and a different result
-- under the same operation id is rejected without mutation.
--
-- inference_tasks.result_digest also records what a non-speaker result staged,
-- so every commit can prove which bytes it published. Existing rows keep NULL:
-- a completed result with no recorded digest is a legacy write whose identity
-- cannot be proven, and it is rejected rather than guessed.
ALTER TABLE inference_tasks ADD COLUMN result_digest TEXT;

-- speaker_embedding_staging.result_digest is what was encrypted and staged
-- under secret_ref. The commit transaction compares it against the submitted
-- digest before publishing metadata, so the canonical rows can only ever hold
-- vectors whose ciphertext the Hub itself staged under this identity.
ALTER TABLE speaker_embedding_staging ADD COLUMN result_digest TEXT;
