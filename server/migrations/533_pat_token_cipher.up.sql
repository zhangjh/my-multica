-- Store an encrypted copy of each personal access token so it can be
-- re-displayed on the Settings -> API Tokens page. Rows created before this
-- migration have an empty token_cipher and CANNOT be revealed; only tokens
-- minted after this migration carry the encrypted raw value.
ALTER TABLE personal_access_token
    ADD COLUMN token_cipher TEXT NOT NULL DEFAULT '';