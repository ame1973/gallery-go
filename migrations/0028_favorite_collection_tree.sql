-- Preserve existing collection rows and validate their owner-bound hierarchy.
ALTER TABLE favorite_collections ADD COLUMN version INTEGER NOT NULL DEFAULT 1 CHECK(typeof(version)='integer' AND version>=1);
ALTER TABLE favorite_collections ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0 CHECK(typeof(updated_at)='integer');
UPDATE favorite_collections SET updated_at=created_at;
CREATE INDEX idx_collection_owner_parent_page ON favorite_collections(user_id,parent_id,id);

-- A valid single-parent forest reaches every row from an owned root exactly once.
-- A cross-owner parent, missing parent or cycle is unreachable and aborts migration.
CREATE TEMP TABLE favorite_collection_tree_check(valid INTEGER NOT NULL CHECK(valid=1));
WITH RECURSIVE rooted(user_id,id) AS (
 SELECT user_id,id FROM favorite_collections WHERE parent_id IS NULL
 UNION ALL
 SELECT child.user_id,child.id FROM rooted parent
 JOIN favorite_collections child INDEXED BY idx_collection_owner_parent_page
 ON child.user_id=parent.user_id AND child.parent_id=parent.id
)
INSERT INTO favorite_collection_tree_check(valid)
 SELECT CASE WHEN (SELECT COUNT(*) FROM rooted)=(SELECT COUNT(*) FROM favorite_collections)
 AND NOT EXISTS(SELECT 1 FROM favorite_collections WHERE typeof(id)!='text' OR length(CAST(id AS BLOB)) NOT BETWEEN 1 AND 64 OR instr(id,char(0))!=0)
 THEN 1 ELSE 0 END;
DROP TABLE favorite_collection_tree_check;

CREATE TRIGGER favorite_collection_insert_guard BEFORE INSERT ON favorite_collections BEGIN
 SELECT CASE WHEN NEW.id IS NULL OR typeof(NEW.id)!='text' OR length(CAST(NEW.id AS BLOB)) NOT BETWEEN 1 AND 64 OR instr(NEW.id,char(0))!=0
 THEN RAISE(ABORT,'invalid collection identity') END;
 -- REPLACE must never silently delete an existing collection or its references.
 SELECT CASE WHEN EXISTS(SELECT 1 FROM favorite_collections WHERE id=NEW.id)
 OR EXISTS(SELECT 1 FROM favorite_collections WHERE user_id=NEW.user_id AND COALESCE(parent_id,'')=COALESCE(NEW.parent_id,'') AND name=NEW.name)
 THEN RAISE(ABORT,'collection identity or name already exists') END;
 SELECT CASE WHEN NEW.parent_id IS NOT NULL AND NOT EXISTS(
 SELECT 1 FROM favorite_collections WHERE id=NEW.parent_id AND user_id=NEW.user_id)
 THEN RAISE(ABORT,'collection parent must belong to the same account') END;
END;

CREATE TRIGGER favorite_collection_identity_guard BEFORE UPDATE OF id,user_id ON favorite_collections
WHEN NEW.id IS NOT OLD.id OR NEW.user_id IS NOT OLD.user_id BEGIN
 SELECT RAISE(ABORT,'collection identity and owner are immutable');
END;

CREATE TRIGGER favorite_collection_parent_guard BEFORE UPDATE OF parent_id ON favorite_collections
WHEN NEW.parent_id IS NOT OLD.parent_id AND NEW.parent_id IS NOT NULL BEGIN
 SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM favorite_collections WHERE id=NEW.parent_id AND user_id=OLD.user_id)
 THEN RAISE(ABORT,'collection parent must belong to the same account') END;
 SELECT CASE WHEN EXISTS(
 WITH RECURSIVE ancestors(id) AS (
  SELECT NEW.parent_id
  UNION
  SELECT node.parent_id FROM favorite_collections node JOIN ancestors parent ON node.id=parent.id
  WHERE node.parent_id IS NOT NULL
 ) SELECT 1 FROM ancestors WHERE id=OLD.id)
 THEN RAISE(ABORT,'collection hierarchy cannot contain a cycle') END;
END;

CREATE TRIGGER favorite_collection_name_guard BEFORE UPDATE OF name,parent_id ON favorite_collections
WHEN NEW.name IS NOT OLD.name OR NEW.parent_id IS NOT OLD.parent_id BEGIN
 SELECT CASE WHEN EXISTS(SELECT 1 FROM favorite_collections
 WHERE user_id=OLD.user_id AND COALESCE(parent_id,'')=COALESCE(NEW.parent_id,'') AND name=NEW.name AND id!=OLD.id)
 THEN RAISE(ABORT,'collection name already exists in this parent') END;
END;
