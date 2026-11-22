-- Forward names for the stored values Homelab 0.50.x reads (platform-2jlnx7.14).
--
-- The rename (platform-2jlnx7.1) changed the spelling of values the 0.49.19
-- server stored. The owner set the direction on 2026-11-21: no legacy bridge,
-- no dual reader, no restored old spelling; stored values move forward. This
-- migration moves them, in the one transaction of the release's database leg,
-- before the 0.50.x image takes traffic (SchemaFloor 199). 0.49.19 is not a
-- rollback target after it: the exits from a failed 0.50.x start are a forward
-- fix or a restore of the pre-199 restore point.
--
-- What moves (old -> new), each counted before and after:
--   * advanced_issuer_keys / advanced_capability_issuances.issuer_id
--     techstack.<uuid> -> homelab.<uuid> (the installation label the Delivery
--     journal attests) and the bare issuer techstack -> homelab;
--   * failure_resolution_outcomes.procedure_ref  techstack:<ref> -> homelab:<ref>,
--     and its CHECK admits stackkits|homelab (0.50.x writes homelab:);
--   * operator_self_disclosures.source  techstack -> homelab, and its CHECK
--     admits exactly what 0.50.x writes and reads (link, homelab, legacy cloud);
--   * user_onboarding_state.journey_id  techstack.platform -> homelab.platform;
--   * in every json/jsonb column of a public table: the string values
--     techstack.<id> -> homelab.<id> for the exact ids below (schema ids, the
--     onboarding journey, step and predicate ids), and the object keys
--     techstack_id -> kit_deployment_id and techstack_enrollment ->
--     homelab_enrollment;
--   * the VM lease resource-generation digest. It is a SHA-256 whose domain
--     marker was renamed (techstack.vmlease.resource-generation/v1 ->
--     homelab.vmlease.resource-generation/v1, pkg/vmleases/resource_generation.go),
--     and the marker itself is not stored, so the stored digests are
--     recomputed: for every lease the 0.49.19 digest of its current generation
--     becomes the 0.50.x digest of the same generation, in the decommission
--     claim (techstack_vm_leases.lease_json metadata), in the append-only
--     operation journal and wherever a JSON document holds it as a string
--     (job payloads and results, transition evidence). Every stored claim must
--     equal the 0.49.19 digest computed here, which also proves this SQL
--     encodes the payload exactly as Go did; a digest of an earlier generation
--     matched nothing under 0.49.19 either and stays.
-- The id list is the set of stored spellings that no longer occur in the 0.50.x
-- sources (homelab/docs/rename-boundary-literals.tsv, platform-2jlnx7.14). Only
-- an exact quoted JSON string or key is replaced, so a value that is exactly one
-- of these tokens is renamed even when a person typed it; text inside another
-- string is escaped in the JSON text form and never matches. Columns that a
-- separate digest column covers (provider_desired_spec_revisions.spec_json,
-- provider_operations.command_json, provider_operation_receipts.receipt_json,
-- typed_agent_command_revisions.result_json) are swept like any other: their
-- readers resolve the digest from its column and never rehash the stored JSON.
--
-- What stays on purpose (0.50.x still reads and writes the old spelling; the
-- table and role rename of platform-2jlnx7.7 moves them): techstack_* tables,
-- functions and roles, user_onboarding_state.product = techstack,
-- techstack.provider-control-operation/v1 and the other techstack.*/v1 ids
-- applied migrations enforce, execution_authority techstack_provider_control,
-- techstack.household.* and techstack-registry. The techstack_archive schema
-- keeps its retained rows as recorded. advanced_trust_bindings keeps the issuer
-- 0.49.19 bound: its bundle digest names the old issuer, so 0.50.x refuses it
-- either way and the host's trust re-import creates the binding again.
--
-- After the move the database refuses every 0.49.19 spelling. 0.49.19 serves
-- until the 0.50.x image takes traffic, and the pre-slot schema publication
-- runs before that switch; whatever 0.49.19 would write in that window under
-- an old spelling (a job result, an onboarding record, a disclosure, a
-- decommission claim or journal entry under the old digest) is refused at
-- write instead of being stored for 0.50.x to misread. The constraints admit
-- exactly what 0.50.x writes, so they are its invariants, not a second path:
--   * every json/jsonb column of a public table: no old id and no old key;
--   * the issuer, procedure, source and journey columns: no old value;
--   * the lease decommission claim: never the 0.49.19 digest of its lease;
--   * the operation journal: an insert carrying the 0.49.19 digest of its
--     lease is refused by a trigger.
--
-- A rewrite is a change of spelling, not a domain event: each table the
-- migration changes has its enabled user triggers (updated_at stamps, outbox,
-- append-only and monotonic guards) disabled for the rewrite and restored to
-- their exact mode, and a FORCE ROW LEVEL SECURITY table is opened for the
-- owner and forced again before commit. Every candidate table is locked ACCESS
-- EXCLUSIVE until commit, so the counts are exact and the old server neither
-- reads nor writes while the leg runs. A row RLS keeps from the owner, a table
-- the owner cannot open, an object that would carry both the old and the new
-- key, an onboarding record that already has a homelab.platform twin, a claim
-- that matches no 0.49.19 digest, or any old value left after the rewrite
-- fails the migration and so the release; nothing is skipped silently. The
-- counts are printed as NOTICE lines with the prefix homelab_forward_names
-- (table, column, before, changed, after); no row value is printed.
SET LOCAL lock_timeout = '5s';
-- The sweep reads every json column of every table in one statement. The
-- session bound of two minutes per statement stopped it on production on
-- 2026-11-22 (run 45008979327, rolled back whole). This leg holds the locks
-- on purpose, so the bound is lifted for this transaction only.
SET LOCAL statement_timeout = '0';

ALTER TABLE failure_resolution_outcomes DROP CONSTRAINT failure_resolution_outcomes_procedure_ref_check;
ALTER TABLE operator_self_disclosures DROP CONSTRAINT operator_self_disclosures_source_check;

-- An object that already carries the new key next to the old one (two outbox
-- payloads on production, 2026-11-22) keeps the new key: the old twin is
-- removed before the rename, so the rename can never produce a duplicate key.
CREATE FUNCTION public.homelab_forward_drop_twin_key(doc jsonb, old_key text, new_key text) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE AS $twin$
BEGIN
    IF jsonb_typeof(doc) = 'object' THEN
        RETURN coalesce((SELECT jsonb_object_agg(e.key, public.homelab_forward_drop_twin_key(e.value, old_key, new_key))
                           FROM jsonb_each(doc) AS e
                          WHERE NOT (e.key = old_key AND doc ? new_key)), '{}'::jsonb);
    ELSIF jsonb_typeof(doc) = 'array' THEN
        RETURN coalesce((SELECT jsonb_agg(public.homelab_forward_drop_twin_key(a.value, old_key, new_key) ORDER BY a.ord)
                           FROM jsonb_array_elements(doc) WITH ORDINALITY AS a(value, ord)), '[]'::jsonb);
    END IF;
    RETURN doc;
END
$twin$;

DO $forward_names$
DECLARE
    -- techstack.<suffix> -> homelab.<suffix>
    suffixes CONSTANT text[] := ARRAY[
        'agent-mtls-enrollment/v1'
        ,'appLogoOverrides.v1'
        ,'application-transfer-step/v1'
        ,'artifact-admission/v1'
        ,'artifact-index/v1'
        ,'backup-owner/v1'
        ,'controller-address-planner/v1'
        ,'creation-entry/v1'
        ,'creation-event/v1'
        ,'credential-preparation/v1'
        ,'database-contract/v1'
        ,'database-delivery/v1'
        ,'device-readiness/v1'
        ,'execution-qualification/v1'
        ,'federation-handoff-result/v1'
        ,'forward-inspect-posture/v1'
        ,'guard.command-binding-key/v1'
        ,'homelab-identity-join-result/v1'
        ,'identity-join-request-input/v1'
        ,'installed-preview-candidate/v1'
        ,'installer-admission/v1'
        ,'job-execution-reclaim/v1'
        ,'kombify.io/layout-problem/v1'
        ,'kombify.io/layout-result/v1'
        ,'kombify.io/layout-snapshot/v1'
        ,'kombify.io/recommendation/v1'
        ,'local-stackkit-teardown-authority/v2'
        ,'merge-expand/v1'
        ,'no-apply-stack-removal/v1'
        ,'offline-stack-removal/v1'
        ,'orphan-projection-cleanup/v1'
        ,'port-teardown-snapshot/v2'
        ,'private-backup-journey/v1'
        ,'provider-image-receipt/v1'
        ,'restore-activation-request/v1'
        ,'ril-action-step-receipt/v1'
        ,'ril-execution-admission/v2'
        ,'ril-lifecycle-receipt/v1'
        ,'ril-queue-proof/v1'
        ,'runtime-candidate/v1'
        ,'runtime-lifecycle/v1'
        ,'runtimeinventory-generation-manifest/v1'
        ,'runtimeinventory-schema/v1'
        ,'schema-expand-company.v1'
        ,'service-action-receipt/v1'
        ,'stackkit-lifecycle-result/v1'
        ,'stackkit-preview-candidate-pin/v1'
        ,'stackkit-release-pin/v1'
        ,'stackkit-release-pin/v2'
        ,'stackkits-kit-metadata/v1'
        ,'vmlease.resource-generation/v1'
        ,'wallet-migration-execute.v1'
        ,'wallet-migration-plan.v1'
        ,'wallet.reveal.v1'
        ,'wizard-extend-receipt/v1'
        ,'wizard-extend/v1'
        ,'wizard-intent/v1'
        ,'workload-relocation-result/v1'
        ,'platform'
        ,'configure_intent'
        ,'add_server'
        ,'deploy_stackkit'
        ,'deploy_service'
        ,'intent_draft_exists'
        ,'kit_deployment_exists'
        ,'server_registered'
        ,'service_running'
    ];
    old_keys CONSTANT text[] := ARRAY['techstack_id', 'techstack_enrollment'];
    new_keys CONSTANT text[] := ARRAY['kit_deployment_id', 'homelab_enrollment'];
    issuer_tables CONSTANT text[] := ARRAY['advanced_capability_issuances', 'advanced_issuer_keys'];
    named_tables CONSTANT text[] := ARRAY['advanced_capability_issuances', 'advanced_issuer_keys',
        'failure_resolution_outcomes', 'operator_self_disclosures', 'techstack_vm_lease_operation_journal',
        'techstack_vm_leases', 'user_onboarding_state'];
    old_digest_version CONSTANT text := 'techstack.vmlease.resource-generation/v1';
    new_digest_version CONSTANT text := 'homelab.vmlease.resource-generation/v1';
    -- A quoted lowercase SHA-256 inside a JSON text, the prefilter of the digest sweep.
    digest_token CONSTANT text := '"[0-9a-f]{64}"';
    bs CONSTANT text := chr(92);
    me oid;
    value_pattern text;
    key_pattern text;
    rewrite_template text;
    -- The 0.49.19/0.50.x digest as SQL over a lease document ({doc}) and a
    -- version literal ({version}). Go: strings.TrimSpace on each field,
    -- strings.ToLower on the provider, json.Marshal of the fixed struct
    -- (HTML-safe escaping of < > & and U+2028/U+2029), sha256, lowercase hex.
    go_space text;
    go_trim text;
    go_json text;
    lease_fields text[];
    digest_template text;
    digest_applicable text;
    claim_sql CONSTANT text := 'coalesce(l.lease_json->''metadata''->>''resource_decommission_claim_digest'', '''')';
    old_digest_sql text;
    new_digest_sql text;
    digest_leases text[] := '{}';
    digest_tenants text[] := '{}';
    old_digests text[] := '{}';
    new_digests text[] := '{}';
    digest_patterns text[] := '{}';
    claims_total bigint := 0;
    claims_old bigint := 0;
    journal_before bigint := 0;
    rel record;
    col record;
    trg record;
    doc record;
    doc_text text;
    unforced oid[] := '{}';
    plan_rel oid[] := '{}';
    plan_col name[] := '{}';
    plan_type text[] := '{}';
    plan_before bigint[] := '{}';
    digest_rel oid[] := '{}';
    digest_col name[] := '{}';
    digest_type text[] := '{}';
    digest_before bigint[] := '{}';
    touched oid[] := '{}';
    trig_rel oid[] := '{}';
    trig_name name[] := '{}';
    trig_mode "char"[] := '{}';
    matched bigint;
    changed bigint;
    remaining bigint;
    total_before bigint := 0;
    total_changed bigint := 0;
    i integer;
    j integer;
    t text;
BEGIN
    SELECT oid INTO STRICT me FROM pg_catalog.pg_roles WHERE rolname = current_user;

    SELECT '"techstack' || bs || '.(' || string_agg(replace(s, '.', bs || '.'), '|') || ')"' INTO value_pattern FROM unnest(suffixes) AS s;
    key_pattern := '"(' || array_to_string(old_keys, '|') || ')"[[:space:]]*:';
    rewrite_template := '{column}::text';
    FOREACH t IN ARRAY suffixes LOOP
        rewrite_template := 'replace(' || rewrite_template || ', ' || quote_literal('"techstack.' || t || '"')
            || ', ' || quote_literal('"homelab.' || t || '"') || ')';
    END LOOP;
    FOR i IN 1 .. array_length(old_keys, 1) LOOP
        rewrite_template := 'regexp_replace(' || rewrite_template || ', ' || quote_literal('"' || old_keys[i] || '"([[:space:]]*):')
            || ', ' || quote_literal('"' || new_keys[i] || '"' || bs || '1:') || ', ''g'')';
    END LOOP;

    -- unicode.IsSpace, the set strings.TrimSpace removes.
    SELECT string_agg(format('chr(%s)', cp), '||' ORDER BY cp) INTO go_space
      FROM unnest(ARRAY[9, 10, 11, 12, 13, 32, 133, 160, 5760, 8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199, 8200,
                        8201, 8202, 8232, 8233, 8239, 8287, 12288]) AS cp;
    go_trim := 'btrim(coalesce(%s, ''''), ' || go_space || ')';
    -- to_json already writes \" \\ \b \f \n \r \t and \u00xx in lowercase, as Go does.
    go_json := 'replace(replace(replace(replace(replace(to_json((%s)::text)::text,''<'', chr(92)||''u003c''), ''>'', chr(92)||''u003e''), '
        || '''&'', chr(92)||''u0026''), chr(8232), chr(92)||''u2028''), chr(8233), chr(92)||''u2029'')';
    lease_fields := ARRAY[
        format(go_trim, '{doc}->''subject''->>''org_id'''),
        format(go_trim, '{doc}->>''id'''),
        format(go_trim, '{doc}->''metadata''->>''resource_generation_id'''),
        'lower(' || format(go_trim, '{doc}->''resource''->>''provider_id''') || ')',
        format(go_trim, '{doc}->''resource''->>''engine_vm_id'''),
        format(go_trim, '{doc}->''resource''->>''simulation_id'''),
        format(go_trim, '{doc}->''resource''->>''vm_id''')];
    digest_template := 'encode(sha256(convert_to(''{"version":'' || ' || format(go_json, '{version}')
        || ' || '',"tenant_id":'' || ' || format(go_json, lease_fields[1])
        || ' || '',"lease_id":'' || ' || format(go_json, lease_fields[2])
        || ' || '',"resource_generation_id":'' || ' || format(go_json, lease_fields[3])
        || ' || '',"provider_id":'' || ' || format(go_json, lease_fields[4])
        || ' || '',"engine_vm_id":'' || ' || format(go_json, lease_fields[5])
        || ' || '',"simulation_id":'' || ' || format(go_json, lease_fields[6])
        || ' || '',"vm_id":'' || ' || format(go_json, lease_fields[7])
        || ' || ''}'', ''UTF8'')), ''hex'')';
    -- Go computes a digest only when tenant, lease, generation and provider are set.
    digest_applicable := lease_fields[1] || ' <> '''' AND ' || lease_fields[2] || ' <> '''' AND '
        || lease_fields[3] || ' <> '''' AND ' || lease_fields[4] || ' <> ''''';
    old_digest_sql := replace(replace(digest_template, '{version}', quote_literal(old_digest_version)), '{doc}', 'l.lease_json');
    new_digest_sql := replace(replace(digest_template, '{version}', quote_literal(new_digest_version)), '{doc}', 'l.lease_json');

    -- 1. Open and lock every candidate table. A row the owner cannot see would
    -- stay under the old spelling, so a table it cannot open refuses. ACCESS
    -- EXCLUSIVE until commit: the counts below are exact, the constraints at
    -- the end are added under the same lock, and the old server waits.
    FOR rel IN
        SELECT c.oid, c.relname, c.relowner, c.relrowsecurity, c.relforcerowsecurity
          FROM pg_catalog.pg_class c
          JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname <> 'schema_migrations'
           AND (c.relname = ANY (named_tables) OR EXISTS (
                SELECT 1 FROM pg_catalog.pg_attribute a
                 WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
                   AND a.attgenerated = '' AND a.atttypid IN ('json'::regtype, 'jsonb'::regtype)))
         ORDER BY c.relname
    LOOP
        IF rel.relrowsecurity AND rel.relowner <> me THEN
            RAISE EXCEPTION 'homelab_forward_names: % has row level security and is not owned by %', rel.relname, current_user;
        END IF;
        EXECUTE format('LOCK TABLE public.%I IN ACCESS EXCLUSIVE MODE', rel.relname);
        IF rel.relforcerowsecurity THEN
            EXECUTE format('ALTER TABLE public.%I NO FORCE ROW LEVEL SECURITY', rel.relname);
            unforced := unforced || rel.oid;
        END IF;
    END LOOP;
    FOREACH t IN ARRAY named_tables LOOP
        IF pg_catalog.to_regclass('public.' || t) IS NULL THEN
            RAISE EXCEPTION 'homelab_forward_names: table % is missing', t;
        END IF;
    END LOOP;

    -- 2a. The lease digests: the 0.49.19 and the 0.50.x digest of every lease's
    -- current generation. Each stored claim must be the 0.49.19 one.
    EXECUTE format('SELECT count(*) FROM public.techstack_vm_leases l WHERE %s AND octet_length(%s) <> length(%s)',
        replace(digest_applicable, '{doc}', 'l.lease_json'), replace(lease_fields[4], '{doc}', 'l.lease_json'),
        replace(lease_fields[4], '{doc}', 'l.lease_json')) INTO remaining;
    IF remaining > 0 THEN
        -- Go lowercases Unicode; SQL lower() matches it only for ASCII.
        RAISE EXCEPTION 'homelab_forward_names: % leases name a provider outside ASCII', remaining;
    END IF;
    EXECUTE format('SELECT coalesce(array_agg(l.id ORDER BY l.id), ''{}''), coalesce(array_agg(l.tenant_id ORDER BY l.id), ''{}''),
            coalesce(array_agg(%s ORDER BY l.id), ''{}''), coalesce(array_agg(%s ORDER BY l.id), ''{}'')
          FROM public.techstack_vm_leases l WHERE %s',
        old_digest_sql, new_digest_sql, replace(digest_applicable, '{doc}', 'l.lease_json'))
        INTO digest_leases, digest_tenants, old_digests, new_digests;
    SELECT coalesce(array_agg('%"' || d || '"%'), '{}') INTO digest_patterns FROM unnest(old_digests) AS d;
    EXECUTE format('SELECT count(*) FILTER (WHERE %1$s <> ''''), count(*) FILTER (WHERE %1$s <> '''' AND %1$s = %2$s)
          FROM public.techstack_vm_leases l', claim_sql, old_digest_sql) INTO claims_total, claims_old;
    IF claims_total <> claims_old THEN
        RAISE EXCEPTION 'homelab_forward_names: % of % decommission claims match no 0.49.19 resource generation digest',
            claims_total - claims_old, claims_total;
    END IF;
    SELECT count(*) INTO journal_before
      FROM public.techstack_vm_lease_operation_journal jr
      JOIN unnest(digest_leases, digest_tenants, old_digests) AS m(lease_id, tenant_id, old_digest)
        ON jr.lease_id = m.lease_id AND jr.tenant_id = m.tenant_id AND jr.resource_generation_digest = m.old_digest;
    IF journal_before > 0 THEN
        touched := touched || 'public.techstack_vm_lease_operation_journal'::regclass::oid;
    END IF;

    -- 2b. Count. json/jsonb columns: rows holding an old id as a JSON string or
    -- an old key, and rows holding a 0.49.19 lease digest as a JSON string.
    FOR col IN
        SELECT c.oid AS relid, c.relname, a.attname, pg_catalog.format_type(a.atttypid, NULL) AS coltype
          FROM pg_catalog.pg_class c
          JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
          JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
         WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname <> 'schema_migrations'
           AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
           AND a.atttypid IN ('json'::regtype, 'jsonb'::regtype)
         ORDER BY c.relname, a.attname
    LOOP
        EXECUTE format('SELECT count(*) FROM public.%1$I WHERE %2$I::text LIKE ''%%techstack%%'' AND (%2$I::text ~ %3$L OR %2$I::text ~ %4$L)',
            col.relname, col.attname, value_pattern, key_pattern) INTO matched;
        IF matched > 0 THEN
            FOR i IN 1 .. array_length(old_keys, 1) LOOP
                EXECUTE format('SELECT count(*) FROM public.%1$I WHERE %2$I::text LIKE %3$L AND jsonb_path_exists(%2$I::jsonb, %4$L::jsonpath)',
                    col.relname, col.attname, '%' || old_keys[i] || '%',
                    'lax $.** ? (exists(@."' || old_keys[i] || '") && exists(@."' || new_keys[i] || '"))') INTO remaining;
                IF remaining > 0 THEN
                    RAISE NOTICE 'homelab_forward_names table=% column=% twin_keys=% old=% kept=%',
                        col.relname, col.attname, remaining, old_keys[i], new_keys[i];
                END IF;
            END LOOP;
            plan_rel := plan_rel || col.relid;
            plan_col := plan_col || col.attname;
            plan_type := plan_type || col.coltype;
            plan_before := plan_before || matched;
            IF NOT col.relid = ANY (touched) THEN
                touched := touched || col.relid;
            END IF;
        END IF;
        IF cardinality(digest_patterns) > 0 THEN
            EXECUTE format('SELECT count(*) FROM public.%1$I WHERE %2$I::text ~ %3$L AND %2$I::text LIKE ANY ($1)',
                col.relname, col.attname, digest_token) USING digest_patterns INTO matched;
            IF matched > 0 THEN
                digest_rel := digest_rel || col.relid;
                digest_col := digest_col || col.attname;
                digest_type := digest_type || col.coltype;
                digest_before := digest_before || matched;
                IF NOT col.relid = ANY (touched) THEN
                    touched := touched || col.relid;
                END IF;
            END IF;
        END IF;
    END LOOP;

    FOREACH t IN ARRAY issuer_tables LOOP
        EXECUTE format('SELECT count(*) FROM public.%I WHERE issuer_id = ''techstack'' OR issuer_id LIKE ''techstack.%%''', t) INTO matched;
        IF matched > 0 AND NOT pg_catalog.to_regclass('public.' || t)::oid = ANY (touched) THEN
            touched := touched || pg_catalog.to_regclass('public.' || t)::oid;
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM failure_resolution_outcomes WHERE procedure_ref LIKE 'techstack:%')
       AND NOT 'public.failure_resolution_outcomes'::regclass::oid = ANY (touched) THEN
        touched := touched || 'public.failure_resolution_outcomes'::regclass::oid;
    END IF;
    IF EXISTS (SELECT 1 FROM operator_self_disclosures WHERE source = 'techstack')
       AND NOT 'public.operator_self_disclosures'::regclass::oid = ANY (touched) THEN
        touched := touched || 'public.operator_self_disclosures'::regclass::oid;
    END IF;
    IF EXISTS (SELECT 1 FROM user_onboarding_state WHERE journey_id = 'techstack.platform')
       AND NOT 'public.user_onboarding_state'::regclass::oid = ANY (touched) THEN
        touched := touched || 'public.user_onboarding_state'::regclass::oid;
    END IF;
    SELECT count(*) INTO remaining
      FROM user_onboarding_state old_state
      JOIN user_onboarding_state new_state
        ON new_state.tenant_id = old_state.tenant_id AND new_state.owner_subject_id = old_state.owner_subject_id
       AND new_state.product = old_state.product AND new_state.journey_id = 'homelab.platform'
     WHERE old_state.journey_id = 'techstack.platform';
    IF remaining > 0 THEN
        -- The newer record wins; the old twin is removed before the rename below.
        RAISE NOTICE 'homelab_forward_names table=user_onboarding_state column=journey_id twins=% kept=homelab.platform', remaining;
    END IF;

    -- 3. Hold the enabled user triggers of every table that changes.
    FOR trg IN
        SELECT tg.tgrelid, tg.tgname, tg.tgenabled
          FROM pg_catalog.pg_trigger tg
         WHERE tg.tgrelid = ANY (touched) AND NOT tg.tgisinternal AND tg.tgenabled <> 'D'
         ORDER BY tg.tgrelid, tg.tgname
    LOOP
        EXECUTE format('ALTER TABLE %s DISABLE TRIGGER %I', trg.tgrelid::regclass, trg.tgname);
        trig_rel := trig_rel || trg.tgrelid;
        trig_name := trig_name || trg.tgname;
        trig_mode := trig_mode || trg.tgenabled;
    END LOOP;

    -- 4. Rewrite, then count again.
    FOR i IN 1 .. coalesce(array_length(plan_rel, 1), 0) LOOP
        FOR j IN 1 .. array_length(old_keys, 1) LOOP
            EXECUTE format('UPDATE %1$s SET %2$I = public.homelab_forward_drop_twin_key(%2$I::jsonb, %3$L, %4$L)::%5$s WHERE %2$I::text LIKE %6$L AND jsonb_path_exists(%2$I::jsonb, %7$L::jsonpath)',
                plan_rel[i]::regclass, plan_col[i], old_keys[j], new_keys[j], plan_type[i], '%' || old_keys[j] || '%',
                'lax $.** ? (exists(@."' || old_keys[j] || '") && exists(@."' || new_keys[j] || '"))');
        END LOOP;
        -- A row whose only old spelling was the removed twin no longer needs the rewrite.
        EXECUTE format('SELECT count(*) FROM %1$s WHERE %2$I::text LIKE ''%%techstack%%'' AND (%2$I::text ~ %3$L OR %2$I::text ~ %4$L)',
            plan_rel[i]::regclass, plan_col[i], value_pattern, key_pattern) INTO matched;
        plan_before[i] := matched;
        EXECUTE format('UPDATE %1$s SET %2$I = (%3$s)::%4$s WHERE %2$I::text LIKE ''%%techstack%%'' AND (%2$I::text ~ %5$L OR %2$I::text ~ %6$L)',
            plan_rel[i]::regclass, plan_col[i], replace(rewrite_template, '{column}', quote_ident(plan_col[i])), plan_type[i],
            value_pattern, key_pattern);
        GET DIAGNOSTICS changed = ROW_COUNT;
        EXECUTE format('SELECT count(*) FROM %1$s WHERE %2$I::text LIKE ''%%techstack%%'' AND (%2$I::text ~ %3$L OR %2$I::text ~ %4$L)',
            plan_rel[i]::regclass, plan_col[i], value_pattern, key_pattern) INTO remaining;
        RAISE NOTICE 'homelab_forward_names table=% column=% before=% changed=% after=%',
            plan_rel[i]::regclass, plan_col[i], plan_before[i], changed, remaining;
        IF remaining <> 0 OR changed <> plan_before[i] THEN
            RAISE EXCEPTION 'homelab_forward_names: %.% kept % old values (changed % of %)',
                plan_rel[i]::regclass, plan_col[i], remaining, changed, plan_before[i];
        END IF;
        total_before := total_before + plan_before[i];
        total_changed := total_changed + changed;
    END LOOP;

    FOREACH t IN ARRAY issuer_tables LOOP
        EXECUTE format('SELECT count(*) FROM public.%I WHERE issuer_id = ''techstack'' OR issuer_id LIKE ''techstack.%%''', t) INTO matched;
        EXECUTE format('UPDATE public.%I SET issuer_id = CASE WHEN issuer_id = ''techstack'' THEN ''homelab'' ELSE ''homelab.'' || substr(issuer_id, 11) END
             WHERE issuer_id = ''techstack'' OR issuer_id LIKE ''techstack.%%''', t);
        GET DIAGNOSTICS changed = ROW_COUNT;
        EXECUTE format('SELECT count(*) FROM public.%I WHERE issuer_id = ''techstack'' OR issuer_id LIKE ''techstack.%%''', t) INTO remaining;
        RAISE NOTICE 'homelab_forward_names table=% column=issuer_id before=% changed=% after=%', t, matched, changed, remaining;
        IF remaining <> 0 THEN
            RAISE EXCEPTION 'homelab_forward_names: %.issuer_id kept % old labels', t, remaining;
        END IF;
        total_before := total_before + matched;
        total_changed := total_changed + changed;
    END LOOP;

    SELECT count(*) INTO matched FROM failure_resolution_outcomes WHERE procedure_ref LIKE 'techstack:%';
    UPDATE failure_resolution_outcomes SET procedure_ref = 'homelab:' || substr(procedure_ref, 11)
     WHERE procedure_ref LIKE 'techstack:%';
    GET DIAGNOSTICS changed = ROW_COUNT;
    SELECT count(*) INTO remaining FROM failure_resolution_outcomes WHERE procedure_ref LIKE 'techstack:%';
    RAISE NOTICE 'homelab_forward_names table=failure_resolution_outcomes column=procedure_ref before=% changed=% after=%', matched, changed, remaining;
    total_before := total_before + matched;
    total_changed := total_changed + changed;

    SELECT count(*) INTO matched FROM operator_self_disclosures WHERE source = 'techstack';
    UPDATE operator_self_disclosures SET source = 'homelab' WHERE source = 'techstack';
    GET DIAGNOSTICS changed = ROW_COUNT;
    SELECT count(*) INTO remaining FROM operator_self_disclosures WHERE source = 'techstack';
    RAISE NOTICE 'homelab_forward_names table=operator_self_disclosures column=source before=% changed=% after=%', matched, changed, remaining;
    total_before := total_before + matched;
    total_changed := total_changed + changed;

    DELETE FROM user_onboarding_state old_state USING user_onboarding_state new_state
     WHERE old_state.journey_id = 'techstack.platform' AND new_state.journey_id = 'homelab.platform'
       AND new_state.tenant_id = old_state.tenant_id AND new_state.owner_subject_id = old_state.owner_subject_id
       AND new_state.product = old_state.product;
    SELECT count(*) INTO matched FROM user_onboarding_state WHERE journey_id = 'techstack.platform';
    UPDATE user_onboarding_state SET journey_id = 'homelab.platform' WHERE journey_id = 'techstack.platform';
    GET DIAGNOSTICS changed = ROW_COUNT;
    SELECT count(*) INTO remaining FROM user_onboarding_state WHERE journey_id = 'techstack.platform';
    RAISE NOTICE 'homelab_forward_names table=user_onboarding_state column=journey_id before=% changed=% after=%', matched, changed, remaining;
    IF remaining <> 0 THEN
        RAISE EXCEPTION 'homelab_forward_names: user_onboarding_state kept % techstack.platform records', remaining;
    END IF;
    total_before := total_before + matched;
    total_changed := total_changed + changed;

    -- 4b. The lease digests. A JSON text is rewritten row by row: every exact
    -- quoted 0.49.19 digest becomes the 0.50.x digest of the same lease.
    FOR i IN 1 .. coalesce(array_length(digest_rel, 1), 0) LOOP
        changed := 0;
        FOR doc IN EXECUTE format('SELECT ctid AS row_ctid, %1$I::text AS body FROM %2$s WHERE %1$I::text ~ %3$L AND %1$I::text LIKE ANY ($1)',
                digest_col[i], digest_rel[i]::regclass, digest_token) USING digest_patterns
        LOOP
            doc_text := doc.body;
            FOR j IN 1 .. array_length(old_digests, 1) LOOP
                doc_text := replace(doc_text, '"' || old_digests[j] || '"', '"' || new_digests[j] || '"');
            END LOOP;
            EXECUTE format('UPDATE %1$s SET %2$I = $1::%3$s WHERE ctid = $2', digest_rel[i]::regclass, digest_col[i], digest_type[i])
                USING doc_text, doc.row_ctid;
            changed := changed + 1;
        END LOOP;
        EXECUTE format('SELECT count(*) FROM %1$s WHERE %2$I::text ~ %3$L AND %2$I::text LIKE ANY ($1)',
            digest_rel[i]::regclass, digest_col[i], digest_token) USING digest_patterns INTO remaining;
        RAISE NOTICE 'homelab_forward_names table=% column=% digest before=% changed=% after=%',
            digest_rel[i]::regclass, digest_col[i], digest_before[i], changed, remaining;
        IF remaining <> 0 OR changed <> digest_before[i] THEN
            RAISE EXCEPTION 'homelab_forward_names: %.% kept % 0.49.19 lease digests (changed % of %)',
                digest_rel[i]::regclass, digest_col[i], remaining, changed, digest_before[i];
        END IF;
        total_before := total_before + digest_before[i];
        total_changed := total_changed + changed;
    END LOOP;

    UPDATE public.techstack_vm_lease_operation_journal jr SET resource_generation_digest = m.new_digest
      FROM unnest(digest_leases, digest_tenants, old_digests, new_digests) AS m(lease_id, tenant_id, old_digest, new_digest)
     WHERE jr.lease_id = m.lease_id AND jr.tenant_id = m.tenant_id AND jr.resource_generation_digest = m.old_digest;
    GET DIAGNOSTICS changed = ROW_COUNT;
    SELECT count(*) INTO remaining
      FROM public.techstack_vm_lease_operation_journal jr
      JOIN unnest(digest_leases, digest_tenants, old_digests) AS m(lease_id, tenant_id, old_digest)
        ON jr.lease_id = m.lease_id AND jr.tenant_id = m.tenant_id AND jr.resource_generation_digest = m.old_digest;
    RAISE NOTICE 'homelab_forward_names table=techstack_vm_lease_operation_journal column=resource_generation_digest before=% changed=% after=%',
        journal_before, changed, remaining;
    IF remaining <> 0 OR changed <> journal_before THEN
        RAISE EXCEPTION 'homelab_forward_names: the operation journal kept % 0.49.19 lease digests (changed % of %)',
            remaining, changed, journal_before;
    END IF;
    total_before := total_before + journal_before;
    total_changed := total_changed + changed;

    EXECUTE format('SELECT count(*) FILTER (WHERE %1$s <> '''' AND %1$s = %2$s), count(*) FILTER (WHERE %1$s <> '''' AND %1$s = %3$s)
          FROM public.techstack_vm_leases l', claim_sql, new_digest_sql, old_digest_sql) INTO matched, remaining;
    RAISE NOTICE 'homelab_forward_names table=techstack_vm_leases column=resource_decommission_claim_digest before=% homelab=% after=%',
        claims_old, matched, remaining;
    IF matched <> claims_total OR remaining <> 0 THEN
        RAISE EXCEPTION 'homelab_forward_names: % of % decommission claims carry the 0.50.x digest', matched, claims_total;
    END IF;

    -- 5. Restore every trigger to its exact mode and force row level security
    -- again where it was forced.
    FOR i IN 1 .. coalesce(array_length(trig_rel, 1), 0) LOOP
        EXECUTE format('ALTER TABLE %s ENABLE %s TRIGGER %I', trig_rel[i]::regclass,
            CASE trig_mode[i] WHEN 'A' THEN 'ALWAYS' WHEN 'R' THEN 'REPLICA' ELSE '' END, trig_name[i]);
    END LOOP;
    FOR i IN 1 .. coalesce(array_length(unforced, 1), 0) LOOP
        EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', unforced[i]::regclass);
    END LOOP;
    IF EXISTS (SELECT 1 FROM unnest(unforced) AS u(relid) JOIN pg_catalog.pg_class c ON c.oid = u.relid WHERE NOT c.relforcerowsecurity)
       OR EXISTS (SELECT 1 FROM unnest(trig_rel, trig_name, trig_mode) AS x(relid, tgname, mode)
                  JOIN pg_catalog.pg_trigger tg ON tg.tgrelid = x.relid AND tg.tgname = x.tgname
                  WHERE tg.tgenabled <> x.mode) THEN
        RAISE EXCEPTION 'homelab_forward_names: row level security or trigger state was not restored';
    END IF;

    -- 6. Refuse every 0.49.19 spelling from now on. Each json/jsonb column was
    -- counted at zero above under the same lock, so the constraint is added
    -- NOT VALID instead of reading every column a third time.
    matched := 0;
    FOR col IN
        SELECT c.relname, a.attname
          FROM pg_catalog.pg_class c
          JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
          JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
         WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname <> 'schema_migrations'
           AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
           AND a.atttypid IN ('json'::regtype, 'jsonb'::regtype)
         ORDER BY c.relname, a.attname
    LOOP
        EXECUTE format('ALTER TABLE public.%1$I ADD CONSTRAINT %2$I CHECK (%3$I IS NULL OR %3$I::text NOT LIKE ''%%techstack%%'' OR NOT (%3$I::text ~ %4$L OR %3$I::text ~ %5$L)) NOT VALID',
            col.relname, 'homelab_spelling_' || left(md5(col.relname || '.' || col.attname), 16), col.attname, value_pattern, key_pattern);
        matched := matched + 1;
    END LOOP;
    EXECUTE format('ALTER TABLE public.techstack_vm_leases ADD CONSTRAINT techstack_vm_leases_claim_homelab_digest_check CHECK (%s = '''' OR %s <> %s)',
        replace(claim_sql, 'l.', ''), replace(claim_sql, 'l.', ''), replace(old_digest_sql, 'l.lease_json', 'lease_json'));
    EXECUTE format($refuse$
        CREATE FUNCTION public.techstack_vm_lease_operation_journal_refuse_old_digest()
        RETURNS trigger
        LANGUAGE plpgsql
        SET search_path = pg_catalog
        AS $body$
        BEGIN
            IF NEW.resource_generation_digest IS NOT NULL AND EXISTS (
                SELECT 1 FROM public.techstack_vm_leases l
                 WHERE l.id = NEW.lease_id AND l.tenant_id = NEW.tenant_id
                   AND NEW.resource_generation_digest = %s) THEN
                RAISE EXCEPTION 'VM lease operation journal refuses a 0.49.19 resource generation digest' USING ERRCODE = '23514';
            END IF;
            RETURN NEW;
        END;
        $body$$refuse$, old_digest_sql);
    -- A trigger function needs no EXECUTE grant to fire; no login may call it
    -- (164 already withholds PUBLIC EXECUTE from this owner's new routines).
    EXECUTE 'REVOKE EXECUTE ON FUNCTION public.techstack_vm_lease_operation_journal_refuse_old_digest() FROM PUBLIC';
    CREATE TRIGGER techstack_vm_lease_operation_journal_refuse_old_digest
        BEFORE INSERT ON public.techstack_vm_lease_operation_journal
        FOR EACH ROW EXECUTE FUNCTION public.techstack_vm_lease_operation_journal_refuse_old_digest();

    RAISE NOTICE 'homelab_forward_names total before=% changed=% tables_opened=% tables_changed=% triggers_held=% json_columns_guarded=% leases=% claims=%',
        total_before, total_changed, coalesce(array_length(unforced, 1), 0), coalesce(array_length(touched, 1), 0),
        coalesce(array_length(trig_rel, 1), 0), matched, cardinality(old_digests), claims_total;
END
$forward_names$;

DROP FUNCTION public.homelab_forward_drop_twin_key(jsonb, text, text);

-- The constraints admit exactly what 0.50.x writes; adding them validates every
-- row, so an old spelling left anywhere fails here as well.
ALTER TABLE failure_resolution_outcomes ADD CONSTRAINT failure_resolution_outcomes_procedure_ref_check
    CHECK (procedure_ref ~ '^(stackkits|homelab):[a-z0-9][a-z0-9._:/@#-]{1,199}$');
ALTER TABLE operator_self_disclosures ADD CONSTRAINT operator_self_disclosures_source_check
    CHECK (source IN ('cloud', 'link', 'homelab'));
ALTER TABLE advanced_issuer_keys ADD CONSTRAINT advanced_issuer_keys_issuer_homelab_check
    CHECK (issuer_id <> 'techstack' AND issuer_id NOT LIKE 'techstack.%');
ALTER TABLE advanced_capability_issuances ADD CONSTRAINT advanced_capability_issuances_issuer_homelab_check
    CHECK (issuer_id <> 'techstack' AND issuer_id NOT LIKE 'techstack.%');
ALTER TABLE user_onboarding_state ADD CONSTRAINT user_onboarding_state_journey_homelab_check
    CHECK (journey_id <> 'techstack.platform');
