-- 0002_rag_capture.sql
-- RAG data capture: per-session grouping + per-Step rich rows with
-- redacted free text, tool trajectories, and feedback labels.
--
-- Parallel to the existing `incidents` table (which stays untouched so
-- the single-grep sanitization audit on pkg/telemetry/logger.go keeps
-- passing). Diagnostic Steps write both rows; non-diagnostic chat Steps
-- write only rag_events.

create table if not exists sessions (
    id              uuid primary key,
    cluster_id      text not null,              -- existing 8-byte sha256 hex
    model           text not null,
    client_version  text not null,              -- ldflags-injected build tag
    auto_exec_on    boolean not null default false,
    started_at      timestamptz not null default now()
);

create table if not exists rag_events (
    id                   bigserial primary key,
    session_id           uuid references sessions(id) on delete cascade,
    step_index           int not null,                 -- 0-based within session
    cluster_id           text not null,                -- denormalized for filter queries

    -- Free-text, REDACTED via pkg/telemetry/sanitize.go.
    -- Never raw pod / namespace / image strings.
    user_query_redacted  text not null,
    diagnosis_redacted   text,                         -- nullable: non-diagnostic chat may have no diagnosis

    -- Trajectory
    tool_sequence        text[] not null default '{}', -- ordered list of tool names
    round_trip_count     int not null default 0,       -- Anthropic round-trips inside the Step
    step_kind            text not null,                -- 'diagnostic' | 'chat' | 'write' | 'mixed'

    -- Classification (mirrors incidents when applicable)
    error_type           text,
    confidence           text,                         -- 'High' | 'Medium' | 'Low' | NULL
    incident_id          bigint,                       -- soft link to incidents.id when both rows written

    -- Labels (the moat). feedback = -1 bad, 0 unset, +1 good.
    feedback             smallint not null default 0,
    followup_within_sec  int,                          -- implicit signal: gap until next Step in same session

    -- Auditability of the redaction itself.
    redaction_stats      jsonb,                        -- {pods:3, namespaces:1, images:2, ...}

    created_at           timestamptz not null default now()
);

create index if not exists rag_events_session_step_idx on rag_events (session_id, step_index);
create index if not exists rag_events_cluster_created_idx on rag_events (cluster_id, created_at desc);
create index if not exists rag_events_tool_sequence_gin on rag_events using gin (tool_sequence);
create index if not exists rag_events_error_type_idx on rag_events (error_type) where error_type is not null;
create index if not exists rag_events_feedback_idx on rag_events (feedback) where feedback <> 0;

-- Forward-compatible: when the RAG retrieval stack is chosen (Voyage /
-- OpenAI / Supabase Edge Function), add the embeddings table without any
-- migration to the above. Left commented for documentation only.
--
-- create extension if not exists vector;
-- create table rag_embeddings (
--     rag_event_id bigint primary key references rag_events(id) on delete cascade,
--     embed_query  vector(1024),
--     embed_diag   vector(1024),
--     model        text not null,
--     created_at   timestamptz not null default now()
-- );
