import copy
import unittest
from suite import reconcile_terminal_censor


class TerminalCensorTests(unittest.TestCase):
    def fixture(self):
        measurement = {"status": "failed", "completed_window_evidence_valid": True,
                       "observer_error_count": 1, "reading_count": 57}
        comparison = {"status": "succeeded"}
        audit = {"status": "censored", "censored_group_ids": ["step58"],
                 "primary_metric_group_ids": ["step" + str(i) for i in range(1, 58)],
                 "censored_groups_without_comparison": ["step58"],
                 "censoring_reason": "native deadline during registration"}
        return measurement, comparison, audit

    def test_qualified_censor_preserves_error_and_raw_count(self):
        measurement, comparison, audit = self.fixture()
        reconcile_terminal_censor(measurement, comparison, audit, "audit-hash")
        self.assertEqual(measurement["status"], "censored")
        self.assertEqual(measurement["pre_audit_status"], "failed")
        self.assertEqual(measurement["observer_error_count"], 1)
        self.assertEqual(measurement["reading_count"], 57)
        self.assertEqual(measurement["protocol_complete_llm_steps"], 57)
        self.assertEqual(measurement["censored_groups_without_comparison"], ["step58"])
        self.assertEqual(measurement["terminal_censor_audit_sha256"], "audit-hash")

    def test_does_not_mask_mapping_resource_or_pairing_failure(self):
        measurement, comparison, audit = self.fixture()
        measurement["completed_window_evidence_valid"] = False
        before = copy.deepcopy(measurement)
        reconcile_terminal_censor(measurement, comparison, audit, "hash")
        self.assertEqual(measurement, before)

    def test_requires_evidence_flag_and_successful_comparison(self):
        for mode in ["missing-evidence", "failed-comparison"]:
            with self.subTest(mode=mode):
                measurement, comparison, audit = self.fixture()
                if mode == "missing-evidence":
                    measurement.pop("completed_window_evidence_valid")
                else:
                    comparison["status"] = "failed"
                before = copy.deepcopy(measurement)
                reconcile_terminal_censor(measurement, comparison, audit, "hash")
                self.assertEqual(measurement, before)

    def test_cannot_qualify_failed_or_incomplete_audit(self):
        for mode in ["failed", "missing-censor-ids", "missing-primary-ids"]:
            with self.subTest(mode=mode):
                measurement, comparison, audit = self.fixture()
                if mode == "failed":
                    audit["status"] = "failed"
                elif mode == "missing-censor-ids":
                    audit["censored_group_ids"] = []
                else:
                    audit.pop("primary_metric_group_ids")
                before = copy.deepcopy(measurement)
                reconcile_terminal_censor(measurement, comparison, audit, "hash")
                self.assertEqual(measurement, before)


if __name__ == "__main__":
    unittest.main()
