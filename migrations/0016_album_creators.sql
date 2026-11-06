-- Keep indexed creator provenance separate from a deliberate classification.
-- Existing albums retain their current creator and start in mapped mode.
ALTER TABLE albums ADD COLUMN mapped_person_id TEXT REFERENCES people(id) ON DELETE RESTRICT;
UPDATE albums SET mapped_person_id=person_id;
ALTER TABLE albums ADD COLUMN person_mode TEXT NOT NULL DEFAULT 'mapped' CHECK(person_mode IN ('mapped','person','none'));
ALTER TABLE albums ADD COLUMN person_override_id TEXT REFERENCES people(id) ON DELETE RESTRICT CHECK((person_mode='person' AND person_override_id IS NOT NULL) OR (person_mode IN ('mapped','none') AND person_override_id IS NULL));
CREATE INDEX idx_album_mapped_person ON albums(mapped_person_id,id);
