-- Revision marker for the consuming binary: feature_preferences is no longer
-- read or written. Retain the table until production-wide preference counts
-- and any Administration override carryover are resolved. No schema floor increase.
SELECT 1;
