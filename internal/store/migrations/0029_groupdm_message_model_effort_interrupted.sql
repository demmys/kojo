-- 0029_groupdm_message_model_effort_interrupted.sql
--
-- Adds groupdm_messages.model, groupdm_messages.effort and
-- groupdm_messages.interrupted — the agent's configured model/effort at the
-- time of a thread reply, and whether the turn ended before completion
-- (operator stop, error, or timeout; the content is partial output).
-- NULL for user/system posts and for agent posts made outside a thread turn.
ALTER TABLE groupdm_messages ADD COLUMN model TEXT;
ALTER TABLE groupdm_messages ADD COLUMN effort TEXT;
ALTER TABLE groupdm_messages ADD COLUMN interrupted INTEGER;
