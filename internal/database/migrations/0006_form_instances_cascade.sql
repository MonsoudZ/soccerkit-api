-- Let a form template take its instances with it.
--
-- form_instances.template_id was the one foreign key in the schema with no ON DELETE
-- clause, so it defaulted to NO ACTION. form_templates.organization_id cascades, which
-- means deleting an organization tried to delete its templates and Postgres refused
-- whenever an instance still pointed at one:
--
--   ERROR: update or delete on table "form_templates" violates foreign key constraint
--          "form_instances_template_id_fkey" on table "form_instances"
--
-- That made DELETE /me fail outright — in one transaction, so nothing was erased and
-- every retry failed the same way — for any account that had filed an evaluation which
-- outlives the org deletion. Two ordinary cases do that: an instance whose subject is
-- the caller (handleDeleteMe removes the orgs before the caller's own Person, so the
-- instance is still there when the templates go), and one whose subject is a shared
-- athlete, whom SelectOrphanedAthletePersonIDs deliberately spares. A player submitting
-- their own pre-game check-in is the first case, and it is the product's core loop.
--
-- CASCADE is the semantics the rest of the engine already has: form_fields cascade from
-- the template and form_answers cascade from the field, so deleting a template destroys
-- every answer regardless. Keeping the instance would leave a husk with no fields, no
-- answers and a dangling template id.

ALTER TABLE form_instances
    DROP CONSTRAINT form_instances_template_id_fkey,
    ADD CONSTRAINT form_instances_template_id_fkey
        FOREIGN KEY (template_id) REFERENCES form_templates (id) ON DELETE CASCADE;
