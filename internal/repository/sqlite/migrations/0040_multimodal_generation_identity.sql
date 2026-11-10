-- 0040_multimodal_generation_identity: record which model produced each
-- generation, so a timeline or an evidence row can name its provenance without
-- guessing from the task that happened to run it.
ALTER TABLE modality_generations ADD COLUMN model_id TEXT NOT NULL DEFAULT '';
ALTER TABLE modality_generations ADD COLUMN model_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE modality_generations ADD COLUMN backend TEXT NOT NULL DEFAULT '';
