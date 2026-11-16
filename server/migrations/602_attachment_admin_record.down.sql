-- Refuse to discard recovery recipes after any accepted removal.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM attachment_admin_record WHERE kind='operation') THEN
  RAISE EXCEPTION 'attachment operation receipts require explicit retention review';
 END IF;
END $$;
DROP TABLE IF EXISTS attachment_admin_record;
