import unittest

from replay_windows import assign, identify


class BatchAttributionTests(unittest.TestCase):
    def test_overlapping_members_and_persistent_process_keep_their_own_evidence(self):
        def event(kind, seq, step, operation="Exec", n=0, handle=0, code=0):
            return dict(event=kind,sequence=seq,step_id=step,operation=operation,parallel_size=n,
                        source_process_session_id=handle,exit_code=code,expected_exit_code=code)
        events = [event("tool_dispatch",1,6,"StartProcess",2,11), event("tool_dispatch",2,6,"Exec",2),
                  event("tool_reply",2,6,"Exec",2), event("tool_reply",1,6,"StartProcess",2,11,-1),
                  {"event":"wait_start"}, event("tool_dispatch",3,8,"PollProcess",0,11),
                  event("tool_reply",3,8,"PollProcess",0,11)]
        groups, status = identify(events)
        self.assertEqual(status["open_handles"],[])
        phase, union = assign("tool",6,groups)
        self.assertEqual((phase,union["member_sequences"]),("tool-batch",[1,2]))
        self.assertEqual(assign("wait",8,groups)[0],"wait-with-background")
        self.assertEqual(events[4]["background_handles_possibly_running"],[11])
        self.assertEqual(events[-1]["start_sequence"],1)

    def test_partial_session_preserves_unclosed_start(self):
        base={"step_id":3,"sequence":1,"operation":"StartProcess","source_process_session_id":13}
        events=[dict(base,event="tool_dispatch"),dict(base,event="tool_reply",exit_code=-1,expected_exit_code=-1)]
        _,status=identify(events,complete=False)
        self.assertEqual(status["open_handles"],[13])
        self.assertFalse(events[-1]["terminal_poll_observed"])


if __name__ == "__main__":
    unittest.main()
