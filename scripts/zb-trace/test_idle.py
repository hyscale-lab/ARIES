import unittest
from unittest.mock import patch
import idle


class BatchingTests(unittest.TestCase):
    def test_reads_contiguous_runs_without_physical_holes(self):
        calls = []
        def read(fd, size, offset):
            calls.append((size, offset))
            return b''.join(idle.U64.pack(i) for i in range(offset//8, (offset+size)//8))
        with patch('idle.os.pread', side_effect=read):
            result = idle.words(10, [4, 5, 6, 100000000, 100000001])
        self.assertEqual(calls, [(24, 32), (16, 800000000)])
        self.assertEqual(result, {n:n for n in [4,5,6,100000000,100000001]})

    def test_reads_are_bounded(self):
        self.assertEqual(list(idle.runs(range(7), limit=3)), [(0,3), (3,6), (6,7)])

    def test_short_metadata_read_is_error(self):
        with patch('idle.os.pread', return_value=b''):
            with self.assertRaises(OSError): idle.words(10, [1])

    def test_write_targets_only_eligible_bits(self):
        mapping = {0: {'pfn':64,'unknown':None}, 4096:{'pfn':65,'unknown':None}, 8192:{'pfn':66,'unknown':'shared'}}
        bits = idle.IdleObserver.bits(mapping)
        self.assertEqual(bits, {1:3})
        with patch('idle.os.pwrite', return_value=8) as write:
            idle.write_words(10, bits)
        write.assert_called_once_with(10, idle.U64.pack(3), 8)


    def test_idle_bitmap_write_boundary_512_and_513_words(self):
        for count, sizes in ((512,[4096]),(513,[4096,8])):
            writes=[]
            def write(fd,data,offset):
                writes.append((offset,bytes(data)))
                return min(len(data),4096)  # Actual kernfs binary-file limit.
            values={7+i:(i*17+1) for i in range(count)}
            with patch('idle.os.pwrite',side_effect=write):
                idle.write_words(10,values)
            self.assertEqual([len(data) for _,data in writes],sizes)
            self.assertEqual([offset for offset,_ in writes],[56+sum(sizes[:i]) for i in range(len(sizes))])
            self.assertEqual(b''.join(data for _,data in writes),b''.join(idle.U64.pack(v) for v in values.values()))

    def test_large_idle_bitmap_read_respects_kernfs_cap_without_reading_holes(self):
        calls=[]
        indices=list(range(9,9+2049))+[1000000,1000001]
        def read(fd,size,offset):
            calls.append((size,offset))
            returned=min(size,4096)
            return b''.join(idle.U64.pack(i) for i in range(offset//8,(offset+returned)//8))
        with patch('idle.os.pread',side_effect=read):
            result=idle.words(10,indices,limit=idle.BITMAP_WORDS)
        self.assertEqual(result,{i:i for i in indices})
        self.assertTrue(all(size<=4096 for size,_ in calls))
        self.assertEqual(calls[-1],(16,8000000))
        self.assertEqual(sum(size for size,_ in calls),len(indices)*8)

    def test_proc_metadata_reads_keep_larger_batching(self):
        calls=[]
        def read(fd,size,offset):
            calls.append((size,offset))
            return b''.join(idle.U64.pack(i) for i in range(offset//8,(offset+size)//8))
        with patch('idle.os.pread',side_effect=read):
            result=idle.words(10,range(1024))
        self.assertEqual(calls,[(8192,0)])
        self.assertEqual(len(result),1024)

    def test_short_idle_write_below_kernel_limit_still_fails(self):
        with patch('idle.os.pwrite',return_value=0):
            with self.assertRaisesRegex(OSError,'short idle bitmap write'):
                idle.write_words(10,{7:1})

class ObservationTests(unittest.TestCase):
    def observer(self):
        o = object.__new__(idle.IdleObserver)
        o.pid, o.vm_id, o.start = 123,'vm','456'
        o.idle, o.flags, o.count = 10, 11, 12
        o.identity = lambda: None
        o.armed = {'begin':1,'end':2,'cpu':3,'tool_call_id':'call-a','absent':[[8192,12288]],
                   'before': {0:{'pfn':64,'unknown':None}, 4096:{'pfn':65,'unknown':None}}}
        return o

    def test_touched_idle_new_and_changed_are_distinct(self):
        o = self.observer()
        o.armed['before'][12288] = {'pfn':67,'unknown':None}
        o.inventory = lambda: ({0:64,4096:65,8192:66,12288:99}, [])
        o.mapping = lambda r: {g:{'pfn':p,'unknown':None} for g,p in r.items()}
        with patch('idle.words', return_value={1:2}): result=o.collect()
        self.assertEqual(result['accessed_gpas'], [0])
        self.assertEqual(result['idle_gpas'], [4096])
        self.assertEqual(result['unknown_gpas'], {'8192':'newly-resident-unarmed','12288':'host-mapping-changed'})
        self.assertEqual(result['tool_call_id'], 'call-a')
        self.assertIsNone(o.armed)

    def test_disappeared_and_compound_remain_unknown(self):
        o=self.observer()
        o.armed['before'][0]['unknown']='compound-resolution-unsupported'
        o.inventory=lambda: ({0:64}, [[4096,8192]])
        o.mapping=lambda r: {0:{'pfn':64,'unknown':None}}
        with patch('idle.words', return_value={1:0}): result=o.collect()
        self.assertEqual(result['accessed_gpas'], [])
        self.assertEqual(result['idle_gpas'], [])
        self.assertEqual(result['unknown_gpas'], {'0':'compound-resolution-unsupported','4096':'became-nonresident'})

    def test_second_endpoint_change_is_unknown(self):
        o=self.observer()
        o.inventory=lambda: ({0:64,4096:65}, [])
        o.mapping=iter([{0:{'pfn':64,'unknown':None},4096:{'pfn':65,'unknown':None}},
                        {0:{'pfn':99,'unknown':None},4096:{'pfn':65,'unknown':None}}]).__next__
        next_mapping=o.mapping
        o.mapping=lambda r: next_mapping()
        with patch('idle.words', return_value={1:0}): result=o.collect()
        self.assertEqual(result['unknown_gpas']['0'], 'host-mapping-changed')

    def test_identity_failure_never_returns_empty_success(self):
        o=self.observer()
        o.identity=lambda: (_ for _ in ()).throw(RuntimeError('VMM identity changed'))
        o.inventory=lambda: ({}, [])
        o.mapping=lambda r: {}
        with patch('idle.words', return_value={1:0}):
            with self.assertRaisesRegex(RuntimeError,'identity'): o.collect()

    def test_mapping_rejects_shared_and_non_lru(self):
        o=self.observer()
        with patch('idle.words', side_effect=[{64:idle.LRU|idle.SHARED,65:0},{64:1,65:1}]):
            result=o.mapping({0:64,4096:65})
        self.assertEqual(result[0]['unknown'],'shared-or-zero-page')
        self.assertEqual(result[4096]['unknown'],'not-on-lru')

    def test_collect_applies_bitmap_limit_to_idle_reads(self):
        o=self.observer()
        o.inventory=lambda: ({0:64,4096:65}, [])
        o.mapping=lambda r: {g:{'pfn':p,'unknown':None} for g,p in r.items()}
        with patch('idle.words',return_value={1:0}) as read:
            reading=o.collect()
        self.assertEqual(read.call_args.kwargs,{'limit':idle.BITMAP_WORDS})
        self.assertEqual(reading['accessed_gpas'],[0,4096])

    def test_double_arm_rejected(self):
        with self.assertRaises(RuntimeError): self.observer().arm()

    def test_partial_open_cleanup(self):
        slot={'guest_phys_addr':0,'userspace_addr':4096,'memory_size':4096}
        with patch('idle.start_identity',return_value='1'), patch('idle.os.open',side_effect=[11,PermissionError()]), patch('idle.os.close') as close:
            with self.assertRaises(PermissionError): idle.IdleObserver(1,'vm',[slot])
        close.assert_called_once_with(11)

    def test_slot_overlap_rejected_before_file_open(self):
        slot={'guest_phys_addr':0,'userspace_addr':4096,'memory_size':4096}
        with patch('idle.start_identity',return_value='1'), patch('idle.os.open') as opened:
            with self.assertRaises(ValueError): idle.IdleObserver(1,'vm',[slot,slot])
        opened.assert_not_called()

    def test_inventory_retains_guest_hole_and_nonresident_ranges(self):
        o=self.observer();o.pm=10
        o.slots=[{'guest_phys_addr':0,'userspace_addr':4096,'memory_size':8192},
                 {'guest_phys_addr':16384,'userspace_addr':12288,'memory_size':4096}]
        with patch('idle.os.pread',side_effect=[idle.U64.pack(idle.PRESENT|64)+idle.U64.pack(0),idle.U64.pack(0)]):
            resident,absent=o.inventory()
        self.assertEqual(resident,{0:64})
        self.assertEqual(absent,[[4096,8192],[16384,20480]])


if __name__=='__main__': unittest.main()
