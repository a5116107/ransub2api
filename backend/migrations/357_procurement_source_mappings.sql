-- Declared supplier groups are distinct from local Groups and analytical
-- upstream_channels. This registry does not grant routing or qualification.
CREATE TABLE procurement_supplier_sites (
    id BIGSERIAL PRIMARY KEY,
    code VARCHAR(64) NOT NULL UNIQUE CHECK (code ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE procurement_supplier_groups (
    id BIGSERIAL PRIMARY KEY,
    site_id BIGINT NOT NULL REFERENCES procurement_supplier_sites(id) ON DELETE CASCADE,
    platform VARCHAR(64) NOT NULL CHECK (platform <> ''),
    external_group_id TEXT NOT NULL CHECK (char_length(external_group_id) BETWEEN 1 AND 256),
    external_group_name TEXT NOT NULL DEFAULT '' CHECK (char_length(external_group_name) <= 100),
    local_group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (site_id, platform, external_group_id),
    UNIQUE (id, site_id, platform)
);

CREATE TABLE procurement_supplier_group_accounts (
    id BIGSERIAL PRIMARY KEY,
    source_group_id BIGINT NOT NULL,
    site_id BIGINT NOT NULL,
    platform VARCHAR(64) NOT NULL,
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    route_fingerprint BYTEA NOT NULL CHECK (octet_length(route_fingerprint) = 32),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (source_group_id, site_id, platform)
        REFERENCES procurement_supplier_groups(id, site_id, platform) ON DELETE CASCADE,
    UNIQUE (source_group_id, account_id),
    -- One account route cannot ambiguously claim multiple supplier sites or
    -- fixed groups until request-scoped selection is proven.
    UNIQUE (platform, account_id)
);

CREATE INDEX procurement_supplier_groups_local_group
    ON procurement_supplier_groups (local_group_id, platform, site_id, id);
CREATE INDEX procurement_supplier_groups_search
    ON procurement_supplier_groups (site_id, platform, external_group_id, id);
CREATE INDEX procurement_supplier_group_accounts_account
    ON procurement_supplier_group_accounts (account_id, id);
