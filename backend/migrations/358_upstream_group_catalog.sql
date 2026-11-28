-- One bounded, sanitized catalog per existing encrypted management identity.
-- No credentials or original upstream response bodies are duplicated here.
-- Application rollback leaves this additive column and its observations intact.
ALTER TABLE new_api_site_authorizations ADD COLUMN group_catalog JSONB;
ALTER TABLE new_api_site_authorizations ADD CONSTRAINT new_api_group_catalog_object
    CHECK (group_catalog IS NULL OR (jsonb_typeof(group_catalog) = 'object'
        AND octet_length(group_catalog::text) <= 2097152));
