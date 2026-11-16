-- Preserve the uniqueness guard while recovery records remain.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM attachment_admin_record) THEN
  RAISE EXCEPTION 'attachment admin records still require uniqueness';
 END IF;
END $$;
DROP INDEX IF EXISTS attachment_admin_record_unique;
