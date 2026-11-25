import unittest

from process import verify_foreground_yields


KEYS = ('sequence', 'step_id', 'source_call_id', 'operation',
        'source_process_session_id', 'exit_code', 'expected_exit_code')


class ForegroundYieldTest(unittest.TestCase):
    def setUp(self):
        self.reply = dict(sequence=17, step_id=22, source_call_id='late', operation='Exec',
                          event='tool_reply', monotonic_ns=1, exit_code=-1, expected_exit_code=0)
        self.first = dict(self.reply, diagnostic_process_id=-17, event='diagnostic_process_observation',
                          status_basis='real_process_running_at_source_terminal_reply', monotonic_ns=2)
        self.dispatch = dict(self.first, event='diagnostic_process_dispatch',
                             status_basis='additional_join_after_source_reply', monotonic_ns=3)
        del self.dispatch['exit_code']

    def test_preserves_optional_observation(self):
        identity = dict(sequence=17, diagnostic_process_id=-17)
        self.assertEqual(verify_foreground_yields([self.first], [self.reply], KEYS), [identity])
        for status in (-1, 0, 2):
            result = dict(self.dispatch, event='diagnostic_process_observation', exit_code=status, monotonic_ns=4)
            self.assertEqual(verify_foreground_yields([self.first, self.dispatch, result], [self.reply], KEYS),
                             [dict(identity, bounded_observed_exit_code=status)])
        self.assertEqual(self.reply['exit_code'], -1)

    def test_rejects_unbound_or_reordered_observation(self):
        for first in (dict(self.first, source_process_session_id=17),
                      dict(self.first, diagnostic_process_id=17),
                      dict(self.first, source_call_id='other')):
            with self.assertRaises(ValueError):
                verify_foreground_yields([first], [self.reply], KEYS)
        result = dict(self.dispatch, event='diagnostic_process_observation', exit_code=0, monotonic_ns=2)
        with self.assertRaises(ValueError):
            verify_foreground_yields([self.first, self.dispatch, result], [self.reply], KEYS)


if __name__ == '__main__':
    unittest.main()
