-- Runs a user cancelled before the "cancelled" status existed.
UPDATE runs SET status='cancelled' WHERE status='failed' AND cancel_reason IS NOT NULL AND cancel_by <> '';
