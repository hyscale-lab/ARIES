import unittest
from unittest.mock import patch
import idle
from capture_slots import parse_slot


class ObserverContractTest(unittest.TestCase):
    def observer(self):
        o = object.__new__(idle.IdleObserver)
        o.pid, o.vm_id, o.start = 123, "vm-instance", "456"
        o.idle = 10
        o.armed = (1, 2, {0: {"pfn": 64, "unknown": None},
                          4096: {"pfn": 65, "unknown": None}})
        return o

    def test_host_pfn_reuse_is_unknown_not_membership(self):
        o = self.observer()
        o.mapping = lambda _: {0: {"pfn": 900, "unknown": None},
                               4096: {"pfn": 65, "unknown": None}}
        with patch("idle.word", return_value=0):
            r = o.collect()
        self.assertEqual(r["unknown_gpas"], {"0": "host-mapping-changed"})
        self.assertEqual(r["accessed_gpas"], [4096])
        self.assertEqual(r["vm_instance"], "vm-instance")

    def test_unknown_compound_never_becomes_base_page_hit(self):
        o = self.observer()
        o.armed[2][0]["unknown"] = "compound-resolution-unsupported"
        o.mapping = lambda _: o.armed[2]
        with patch("idle.word", return_value=2):
            r = o.collect()
        self.assertEqual(r["accessed_gpas"], [])
        self.assertEqual(r["idle_gpas"], [4096])
        self.assertEqual(r["unknown_gpas"], {"0": "compound-resolution-unsupported"})

    def test_permission_failure_is_not_empty_page_set(self):
        with patch("idle.start_identity", return_value="1"), patch("idle.os.open", side_effect=PermissionError("denied")):
            with self.assertRaises(PermissionError):
                idle.IdleObserver(123, "vm", [])

    def test_invalid_slot_does_not_open_kernel_files(self):
        with patch("idle.start_identity", return_value="1"), patch("idle.os.open") as opened:
            with self.assertRaises(ValueError):
                idle.IdleObserver(123, "vm", [{"guest_phys_addr": 1, "userspace_addr": 4096, "memory_size": 4096}])
            opened.assert_not_called()

    def test_partial_open_closes_files_on_permission_failure(self):
        with patch("idle.start_identity", return_value="1"), patch("idle.os.open", side_effect=[11, PermissionError("denied")]), patch("idle.os.close") as closed:
            with self.assertRaises(PermissionError):
                idle.IdleObserver(123, "vm", [])
            closed.assert_called_once_with(11)

    def test_nonresident_is_unknown_not_empty(self):
        o = self.observer()
        for m in o.armed[2].values():
            m["unknown"] = "not-resident"
        o.mapping = lambda _: o.armed[2]
        with patch("idle.word", return_value=0):
            r = o.collect()
        self.assertEqual(r["accessed_gpas"], [])
        self.assertEqual(len(r["unknown_gpas"]), 2)
        self.assertEqual(r["idle_gpas"], [])

    def test_double_arm_rejected(self):
        with self.assertRaises(RuntimeError):
            self.observer().arm([0])

    def test_capture_only_successful_ram_registration(self):
        line = "[pid 123] ioctl(4, KVM_SET_USER_MEMORY_REGION, {slot=0, flags=0, guest_phys_addr=0, memory_size=3489660928, userspace_addr=0x7f0000000000}) = 0\n"
        r = parse_slot(line)
        self.assertEqual((r["tid"], r["guest_phys_addr"], r["memory_size"]), (123, 0, 3489660928))
        self.assertIsNone(parse_slot(line.replace("= 0\n", "= -1 EINVAL\n")))
        self.assertIsNone(parse_slot("ioctl(4, KVM_SET_REGS, {rax=123}) = 0"))

    def test_slot_hole_and_overlap_rejected(self):
        o = self.observer()
        o.slots = [{"guest_phys_addr": 0, "userspace_addr": 0x10000, "memory_size": 4096}]
        self.assertEqual(o.hva(0), 0x10000)
        with self.assertRaises(ValueError):
            o.hva(4096)
        o.slots *= 2
        with self.assertRaises(ValueError):
            o.hva(0)


if __name__ == "__main__":
    unittest.main()
