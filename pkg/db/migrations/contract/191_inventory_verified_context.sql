-- kombify:contract objects=public.servers,public.stacks,public.services
-- Closes token-less access of the hosted application login to the inventory
-- (platform-3r69a). Migration 178 kept every restrictive policy below a no-op
-- without a verified context, so a techstack_application session with only a
-- runtime-chosen app.tenant_id still read and wrote any tenant's servers,
-- stacks and services. From 191 every token-less caller runs on its own login
-- (techstack_runtime_system); the application login serves only Edge-verified
-- requests. A missing, forged, expired or foreign-audience token now matches
-- zero rows here. Own-tenant DML and the signed household read scopes are
-- unchanged. Native Local and Guard logins are not techstack_application and
-- are not addressed by these policies.
SET LOCAL lock_timeout = '5s';

ALTER POLICY household_inventory_read ON public.servers
 USING (tenant_id = (SELECT kombify_ctx.org_id())
   OR public.techstack_household_inventory_server(tenant_id,owner_subject_id,id));
ALTER POLICY household_inventory_read ON public.stacks
 USING (tenant_id = (SELECT kombify_ctx.org_id()) OR (
   public.techstack_household_inventory_owner(tenant_id,owner_subject_id)
   AND (kombify_ctx.has_scope('techstack.household.collection') OR EXISTS (
     SELECT 1 FROM public.servers authorized_server
     WHERE authorized_server.tenant_id=stacks.tenant_id
       AND authorized_server.owner_subject_id=stacks.owner_subject_id
       AND authorized_server.stack_id=stacks.id
       AND public.techstack_household_inventory_server(authorized_server.tenant_id,authorized_server.owner_subject_id,authorized_server.id)
   ))));
ALTER POLICY household_inventory_read ON public.services
 USING (tenant_id = (SELECT kombify_ctx.org_id()) OR EXISTS (
   SELECT 1 FROM public.servers authorized_server
   WHERE authorized_server.tenant_id=services.tenant_id
     AND authorized_server.id=services.server_id
     AND authorized_server.stack_id=services.stack_id
     AND public.techstack_household_inventory_server(authorized_server.tenant_id,authorized_server.owner_subject_id,authorized_server.id)
 ));

ALTER POLICY household_inventory_insert ON public.servers
 WITH CHECK (tenant_id = (SELECT kombify_ctx.org_id()));
ALTER POLICY household_inventory_update ON public.servers
 USING (tenant_id = (SELECT kombify_ctx.org_id())) WITH CHECK (tenant_id = (SELECT kombify_ctx.org_id()));
ALTER POLICY household_inventory_delete ON public.servers
 USING (tenant_id = (SELECT kombify_ctx.org_id()));
ALTER POLICY household_inventory_insert ON public.stacks
 WITH CHECK (tenant_id = (SELECT kombify_ctx.org_id()));
ALTER POLICY household_inventory_update ON public.stacks
 USING (tenant_id = (SELECT kombify_ctx.org_id())) WITH CHECK (tenant_id = (SELECT kombify_ctx.org_id()));
ALTER POLICY household_inventory_delete ON public.stacks
 USING (tenant_id = (SELECT kombify_ctx.org_id()));
ALTER POLICY household_inventory_insert ON public.services
 WITH CHECK (tenant_id = (SELECT kombify_ctx.org_id()));
ALTER POLICY household_inventory_update ON public.services
 USING (tenant_id = (SELECT kombify_ctx.org_id())) WITH CHECK (tenant_id = (SELECT kombify_ctx.org_id()));
ALTER POLICY household_inventory_delete ON public.services
 USING (tenant_id = (SELECT kombify_ctx.org_id()));
