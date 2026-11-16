-- Test initialization source only. No harness command applies this file.
-- A separately approved dedicated initializer creates one row with bound
-- nonce/schema SHA parameters after applying the pinned official schema.
CREATE TABLE chee199_syn_target (
    singleton boolean NOT NULL CHECK (singleton),
    purpose text NOT NULL CHECK (purpose = 'CHEE-199-DEDICATED-SYN-ONLY'),
    nonce text NOT NULL,
    schema_sha256 text NOT NULL
);
