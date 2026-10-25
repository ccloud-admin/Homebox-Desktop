-- Database layout of Homebox v0.27.1 (SQLite), after all of its migrations.
-- Generated from the public migrations in github.com/sysadminsmedia/homebox.
-- Used by tests only; contains no data.

CREATE TABLE groups
(
    id         uuid               not null
        primary key,
    created_at datetime           not null,
    updated_at datetime           not null,
    name       text               not null,
    currency   text default 'usd' not null
);

CREATE TABLE group_invitation_tokens
(
    id                      uuid              not null
        primary key,
    created_at              datetime          not null,
    updated_at              datetime          not null,
    token                   blob              not null,
    expires_at              datetime          not null,
    uses                    integer default 0 not null,
    group_invitation_tokens uuid
        constraint group_invitation_tokens_groups_invitation_tokens
            references groups
            on delete cascade
);

CREATE TABLE auth_tokens
(
    id               uuid     not null
        primary key,
    created_at       datetime not null,
    updated_at       datetime not null,
    token            blob     not null,
    expires_at       datetime not null,
    user_auth_tokens uuid
        constraint auth_tokens_users_auth_tokens
            references users
            on delete cascade
);

CREATE TABLE auth_roles
(
    id                integer             not null
        primary key autoincrement,
    role              text default 'user' not null,
    auth_tokens_roles uuid
        constraint auth_roles_auth_tokens_roles
            references auth_tokens
            on delete cascade
);

CREATE TABLE notifiers
(
    id         uuid              not null
        primary key,
    created_at datetime          not null,
    updated_at datetime          not null,
    name       text              not null,
    url        text              not null,
    is_active  bool default true not null,
    group_id   uuid              not null
        constraint notifiers_groups_notifiers
            references groups
            on delete cascade,
    user_id    uuid              not null
        constraint notifiers_users_notifiers
            references users
            on delete cascade
);

CREATE TABLE user_groups (
    user_id UUID NOT NULL,
    group_id UUID NOT NULL, role TEXT NOT NULL DEFAULT 'user',
    PRIMARY KEY (user_id, group_id),
    CONSTRAINT user_groups_user_id FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    CONSTRAINT user_groups_group_id FOREIGN KEY (group_id) REFERENCES groups(id) ON DELETE CASCADE
);

CREATE TABLE tags (
	id           uuid     not null
		primary key,
	created_at   datetime not null,
	updated_at   datetime not null,
	name         text     not null,
	description  text,
	color        text,
	group_tags   uuid     not null
		constraint tags_groups_tags
			references groups
			on delete cascade
, icon varchar(255) NULL, tag_children uuid REFERENCES tags(id) ON DELETE SET NULL);

CREATE TABLE entity_types (
			id               uuid     not null primary key,
			created_at       datetime not null,
			updated_at       datetime not null,
			name             text     not null,
			description      text,
			is_location      bool     default false not null,
			icon             text,
			group_entity_types uuid   not null
				constraint entity_types_groups_entity_types
					references groups
					on delete cascade
		, entity_type_default_template uuid
    REFERENCES entity_templates(id) ON DELETE SET NULL);

CREATE TABLE entities (
			id                          uuid                  not null primary key,
			created_at                  datetime              not null,
			updated_at                  datetime              not null,
			name                        text                  not null,
			description                 text,
			import_ref                  text,
			notes                       text,
			quantity                    real    default 1     not null,
			insured                     bool    default false not null,
			archived                    bool    default false not null,
			asset_id                    bigint  default 0     not null,
			sync_child_entity_locations bool    default false not null,
			serial_number               text,
			model_number                text,
			manufacturer                text,
			lifetime_warranty           bool    default false not null,
			warranty_expires            datetime,
			warranty_details            text,
			purchase_date               datetime,
			purchase_from               text,
			purchase_price              real    default 0     not null,
			sold_date                   datetime,
			sold_to                     text,
			sold_price                  real    default 0     not null,
			sold_notes                  text,
			group_entities              uuid                  not null
				constraint entities_groups_entities
					references groups
					on delete cascade,
			entity_type_entities        uuid                  not null
				constraint entities_entity_types_entities
					references entity_types
					on delete restrict,
			entity_children             uuid
				constraint entities_entities_children
					references entities
					on delete set null
		, entity_location_entities uuid
        constraint entities_entities_location_entities
            references entities
            on delete set null);

CREATE TABLE entity_fields (
				id            uuid               not null primary key,
				created_at    datetime           not null,
				updated_at    datetime           not null,
				name          text               not null,
				description   text,
				type          text               not null,
				text_value    text,
				number_value  integer,
				boolean_value bool default false not null,
				time_value    datetime           not null,
				entity_fields uuid
					constraint entity_fields_entities_fields references entities on delete cascade
			);

CREATE TABLE tag_entities (
				tag_id    uuid not null constraint tag_entities_tag_id references tags on delete cascade,
				entity_id uuid not null constraint tag_entities_entity_id references entities on delete cascade,
				primary key (tag_id, entity_id)
			);

CREATE TABLE entity_templates (
				id                        uuid                  not null primary key,
				created_at                datetime              not null,
				updated_at                datetime              not null,
				name                      text                  not null,
				description               text,
				notes                     text,
				default_quantity          real    default 1     not null,
				default_insured           bool    default false not null,
				default_name              text,
				default_description       text,
				default_manufacturer      text,
				default_model_number      text,
				default_lifetime_warranty bool    default false not null,
				default_warranty_details  text,
				include_warranty_fields   bool    default false not null,
				include_purchase_fields   bool    default false not null,
				include_sold_fields       bool    default false not null,
				default_tag_ids           json,
				entity_template_location  uuid constraint entity_templates_entities_location references entities on delete set null,
				group_entity_templates    uuid not null constraint entity_templates_groups_entity_templates references groups on delete cascade
			);

CREATE TABLE "template_fields" (
				id                     uuid               not null primary key,
				created_at             datetime           not null,
				updated_at             datetime           not null,
				name                   text               not null,
				description            text,
				type                   text               not null,
				text_value             text,
				number_value           integer,
				boolean_value          bool default false,
				time_value             datetime,
				entity_template_fields uuid constraint template_fields_entity_templates_fields references entity_templates on delete cascade
			);

CREATE TABLE "maintenance_entries" (
				id             uuid           not null primary key,
				created_at     datetime       not null,
				updated_at     datetime       not null,
				date           datetime,
				scheduled_date datetime,
				name           text           not null,
				description    text,
				cost           real default 0 not null,
				entity_id      uuid           not null constraint maintenance_entries_entities_maintenance_entries references entities on delete cascade
			);

CREATE TABLE "attachments" (
				id                   uuid                                       not null primary key,
				created_at           datetime                                   not null,
				updated_at           datetime                                   not null,
				type                 text    default 'attachment'               not null,
				"primary"            bool    default false                      not null,
				path                 text                                       not null,
				title                text                                       not null,
				mime_type            text    default 'application/octet-stream' not null,
				entity_attachments   uuid constraint attachments_entities_attachments references entities on delete cascade,
				attachment_thumbnail uuid constraint attachments_attachments_thumbnail references "attachments" on delete set null
			);

CREATE TABLE api_keys
(
    id           uuid     not null
        primary key,
    created_at   datetime not null,
    updated_at   datetime not null,
    user_id      uuid     not null
        constraint api_keys_users_api_keys
            references users
            on delete cascade,
    name         text     not null,
    token        blob     not null,
    expires_at   datetime,
    last_used_at datetime
);

CREATE TABLE "users" (
    id UUID NOT NULL,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    name TEXT NOT NULL,
    email TEXT NOT NULL,
    password TEXT,
    is_superuser BOOLEAN NOT NULL DEFAULT false,
    superuser BOOLEAN NOT NULL DEFAULT false,
    activated_on DATETIME,
    oidc_issuer TEXT,
    oidc_subject TEXT,
    default_group_id UUID,
    settings JSON DEFAULT '{}',
    PRIMARY KEY (id),
    CONSTRAINT users_groups_users_default FOREIGN KEY (default_group_id) REFERENCES groups(id) ON DELETE SET NULL
);

CREATE TABLE password_reset_tokens
(
    id         uuid     not null
        primary key,
    created_at datetime not null,
    updated_at datetime not null,
    user_id    uuid     not null
        constraint password_reset_tokens_users_password_reset_tokens
            references users
            on delete cascade,
    token      blob     not null,
    expires_at datetime not null,
    used_at    datetime
);

CREATE TABLE exports
(
    id            uuid                       not null
        primary key,
    created_at    datetime                   not null,
    updated_at    datetime                   not null,
    kind          text     default 'export'  not null
        check (kind in ('export', 'import')),
    status        text     default 'pending' not null
        check (status in ('pending', 'running', 'completed', 'failed')),
    progress      integer  default 0         not null,
    artifact_path text,
    size_bytes    integer  default 0         not null,
    error         text
        check (error is null or length(error) <= 1000),
    group_id      uuid                       not null
        constraint exports_groups_exports
            references groups
            on delete cascade
);

CREATE UNIQUE INDEX group_invitation_tokens_token_key
    on group_invitation_tokens (token);

CREATE UNIQUE INDEX auth_roles_auth_tokens_roles_key
    on auth_roles (auth_tokens_roles);

CREATE UNIQUE INDEX auth_tokens_token_key
    on auth_tokens (token);

CREATE INDEX authtokens_token
    on auth_tokens (token);

CREATE INDEX notifier_group_id
    on notifiers (group_id);

CREATE INDEX notifier_group_id_is_active
    on notifiers (group_id, is_active);

CREATE INDEX notifier_user_id
    on notifiers (user_id);

CREATE INDEX notifier_user_id_is_active
    on notifiers (user_id, is_active);

CREATE INDEX entity_name ON entities(name);

CREATE INDEX entity_manufacturer ON entities(manufacturer);

CREATE INDEX entity_model_number ON entities(model_number);

CREATE INDEX entity_serial_number ON entities(serial_number);

CREATE INDEX entity_archived ON entities(archived);

CREATE INDEX entity_asset_id ON entities(asset_id);

CREATE INDEX idx_attachments_entity_id ON attachments(entity_attachments);

CREATE INDEX idx_attachments_path ON attachments(path);

CREATE INDEX idx_attachments_type ON attachments(type);

CREATE INDEX idx_attachments_thumbnail ON attachments(attachment_thumbnail);

CREATE UNIQUE INDEX api_keys_token_key
    on api_keys (token);

CREATE INDEX apikey_token
    on api_keys (token);

CREATE INDEX apikey_user_id
    on api_keys (user_id);

CREATE UNIQUE INDEX users_email_key ON users(email);

CREATE UNIQUE INDEX users_oidc_issuer_subject_key ON users(oidc_issuer, oidc_subject);

CREATE UNIQUE INDEX password_reset_tokens_token_key
    on password_reset_tokens (token);

CREATE INDEX passwordresettokens_user_id
    on password_reset_tokens (user_id);

CREATE INDEX export_group_id
    on exports (group_id);

CREATE INDEX export_group_id_status
    on exports (group_id, status);

CREATE INDEX idx_entities_location
    on entities (entity_location_entities);

