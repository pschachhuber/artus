---
name: Monthly maintenance
status: active
---

Keep your own home tidy and useful. Work from evidence in the last month of use, not from taste: an error that recurred, a workaround you repeated, a note nobody read, a skill that failed. If nothing needs changing, change nothing and say so.

Before touching anything, run `git -C "$ARTUS_HOME" log --oneline -1` to note the last snapshot; every change you make is recorded and can be reverted from Settings.

1. **Memory.** Rewrite memory.md so it is short, current and free of duplicates. Move detail that is only sometimes relevant into a topic note.
2. **Topics.** Merge notes that cover the same subject, delete ones that are wrong or obsolete, and make sure the first line of each says what it covers (that line is its index entry).
3. **Skills and tools.** For each: was it used, and did it work? Grep recent chats for its name and for errors. Fix what is broken, merge overlaps, and remove what was never used and has no clear purpose. Run each script's simplest safe invocation to confirm it still starts.
4. **Tasks.** List triggers with the schedule tool. Flag tasks that are finished but still active, tasks that keep failing, and triggers that point at nothing.
5. **Housekeeping.** Delete files in tmp/ and uploads/ older than 30 days. Run `artus browser install` to update a downloaded browser, if one is in use.
6. **Expiring access.** If Proton Pass is in use, check with `pass-cli info` whether the agent token is close to expiry and say so prominently in your report.
7. **The harness itself.** You cannot change it. If something in it caused repeated friction this month, describe the problem and the evidence in your report so the user can decide.

End with a short report: what you changed and why, what you left alone, and anything that needs the user.
