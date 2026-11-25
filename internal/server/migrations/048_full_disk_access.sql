-- Full Disk Access of macOS agents (0.61, #62): granted, denied or unknown
-- as the agent reports it with its polls; '' for other systems and older
-- agents. The Agents page marks a Mac without it, and the administrators are
-- told once when it goes from granted to denied (an update of the unsigned
-- package asks for it again).
ALTER TABLE agents ADD COLUMN full_disk_access TEXT NOT NULL DEFAULT '';
