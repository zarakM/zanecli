-- 0003_rls_lockdown
-- Lock the public anon key (baked into released binaries via the SUPABASE_KEY
-- ldflag) down to least-privilege. Applied to the live project on 2026-06-04.
--
-- Before this migration:
--   * sessions/rag_events had RLS enabled with NO policies, so every anon write
--     was denied (403) — the entire RAG capture path silently never worked.
--   * anon held ALL privileges (SELECT/INSERT/UPDATE/DELETE/TRUNCATE) on every
--     table; only the absence of non-INSERT policies on `incidents` kept reads
--     and deletes from leaking. One policy mistake from a public-key holder
--     reading or wiping data.
--   * incidents had a redundant `WITH CHECK (true)` INSERT policy that was
--     OR-combined with the validation policy, nullifying it.
--
-- After: anon can INSERT telemetry, read back only rag_events.id (for the
-- return=representation round-trip in pkg/telemetry.LogRagEvent), and PATCH
-- only the two feedback columns. It cannot read the redacted corpus, cannot
-- delete, and malformed incidents are rejected.
--
-- NOTE: migrations 0001 (incidents) and 0002 (sessions + rag_events) predate
-- this file and were never committed to the repo — the tables already existed
-- in the project. This file documents and reproduces the lockdown only.

-- ---- incidents ----------------------------------------------------------
drop policy if exists "allow anon insert" on public.incidents;

-- ---- sessions -----------------------------------------------------------
drop policy if exists "anon insert sessions" on public.sessions;
create policy "anon insert sessions" on public.sessions
  for insert to anon with check (true);

-- ---- rag_events ---------------------------------------------------------
drop policy if exists "anon insert rag_events" on public.rag_events;
create policy "anon insert rag_events" on public.rag_events
  for insert to anon with check (true);

-- return=representation needs a SELECT policy for the inserted row to come
-- back. The column-level grant below limits anon to reading only `id`, so this
-- does NOT expose the redacted corpus.
drop policy if exists "anon select rag_events" on public.rag_events;
create policy "anon select rag_events" on public.rag_events
  for select to anon using (true);

-- feedback / followup_within_sec patches (PatchFeedback / PatchFollowupGap).
drop policy if exists "anon update rag_events" on public.rag_events;
create policy "anon update rag_events" on public.rag_events
  for update to anon using (true) with check (true);

-- ---- tighten grants to least privilege ----------------------------------
revoke all on public.incidents  from anon;
revoke all on public.sessions   from anon;
revoke all on public.rag_events from anon;

grant insert on public.incidents  to anon;
grant insert on public.sessions   to anon;
grant insert on public.rag_events to anon;

-- only id is readable (for the return=representation id round-trip); corpus
-- content stays private even though a permissive SELECT policy exists.
grant select (id) on public.rag_events to anon;

-- only the two feedback columns are updatable by anon.
grant update (feedback, followup_within_sec) on public.rag_events to anon;

-- bigserial default (nextval) needs sequence access on insert.
grant usage, select on sequence public.rag_events_id_seq to anon;
