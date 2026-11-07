-- Operator-reviewed reusable knowledge: an operator-scope memory that scheduled
-- and external runs of the profile may retrieve. Default 0 keeps every existing
-- memory private to the operator.
ALTER TABLE profile_memories ADD COLUMN reusable INTEGER NOT NULL DEFAULT 0 CHECK(reusable IN (0,1));
UPDATE schema_version SET version=28;
