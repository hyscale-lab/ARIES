import unittest
import compare


def row(pages, vm='vm', **extra):
    return {'event':'step_end','session_id':'task','turn_id':'user-1','step_id':'call',
            'reading':{'vm_instance':vm,'page_bytes':4096,'accessed_gpas':pages,'unknown_gpas':{},'idle_gpas':[],
                       'arm_begin_ns':0,'arm_end_ns':10_000_000,'scan_begin_ns':40_000_000,'scan_end_ns':60_000_000},
            'result':{'begin_monotonic_ns':10_000_000,'end_monotonic_ns':40_000_000},**extra}


class ComparisonTest(unittest.TestCase):
    def test_legacy_auto_mode_does_not_claim_segmented_resets(self):
        summary=compare.summarize([row([0])])
        self.assertEqual(summary['mode'],'legacy')
        self.assertEqual(summary['comparison_unit'],'native_tool_call_window')
        page=compare.render(summary)
        self.assertIn('LEGACY SINGLE-WINDOW OBSERVATIONS',page)
        self.assertNotIn('<strong>EXPERIMENTAL SEGMENTED HOOK INTERVALS.',page)

    def test_directional_overlap_and_empty_union(self):
        r=compare.compare_sets([0,4096],[4096,8192])
        self.assertEqual((r['intersection_pages'],r['new_pages'],r['lost_pages']),(1,1,1))
        self.assertEqual(r['jaccard'],1/3)
        self.assertEqual(r['new_fraction_of_union'],1/3)
        self.assertIsNone(compare.compare_sets([],[])['jaccard'])

    def test_different_vm_and_invalid_windows_not_compared(self):
        s=compare.summarize([row([0]),row([0],vm='other'),row([0],measurement_valid=False)])
        self.assertFalse(s['pairs'][1]['comparable'])
        self.assertIsNone(s['pairs'][1]['intersection_pages'])
        self.assertFalse(s['pairs'][2]['comparable'])

    def test_unknown_does_not_become_idle_or_accessed(self):
        r=row([0]);r['reading']['unknown_gpas']={'4096':'newly-resident-unarmed'}
        s=compare.summarize([r])['steps'][0]
        self.assertEqual((s['accessed_pages'],s['idle_pages'],s['unknown_pages']),(1,0,1))
        self.assertEqual(s['unknown_counts'],{'newly-resident-unarmed':1})
        self.assertEqual(s['accessed_bytes'],4096)

    def test_unknown_union_changes_primary_jaccard_preserves_baseline(self):
        a=row([0]);a['reading']['unknown_gpas']={'4096':'newly-resident-unarmed'}
        b=row([4096]);b['reading']['unknown_gpas']={'8192':'shared-or-zero-page'}
        summary=compare.summarize([a,b])
        self.assertEqual(summary['steps'][0]['counted_touched_gpas'],[0,4096])
        self.assertEqual(summary['pairs'][1]['jaccard'],1/3)
        self.assertEqual(summary['observed_only_pairs'][1]['jaccard'],0)
        self.assertEqual(a['reading']['accessed_gpas'],[0])
        self.assertEqual(summary['steps'][1]['counted_touched_pages'],2)

    def test_start_event_ignored_and_mechanism_boundary_distinguished(self):
        r=row([0]);r.pop('event')
        s=compare.summarize([{'event':'step_begin'},r,row([0])])
        self.assertEqual(len(s['steps']),2)
        self.assertEqual(s['steps'][0]['record_kind'],'mechanism_window_not_native_step')
        self.assertFalse(s['pairs'][1]['comparable'])

    def test_timings_do_not_conflate_tool_and_scan(self):
        s=compare.summarize([row([])])['steps'][0]
        self.assertEqual((s['tool_interval_ms'],s['arm_ms'],s['scan_ms'],s['observation_envelope_ms']),(30,10,20,60))
        self.assertIsNone(s['operation_count'])
        self.assertIsNone(compare.elapsed_ms(9,2))

    def test_bad_page_identity_and_overlap_rejected(self):
        for pages in ([1],[0,0],[-4096]):
            with self.assertRaises(ValueError):compare.summarize([row(pages)])
        r=row([0]);r['reading']['unknown_gpas']={'0':'masked'}
        with self.assertRaises(ValueError):compare.summarize([r])

    def test_html_escapes_record_strings_and_renders_matrix(self):
        r=row([0]);r['step_id']='<script>alert(1)</script>'
        page=compare.render(compare.summarize([r,row([4096])]))
        self.assertNotIn('<script>',page)
        self.assertIn('&lt;script&gt;',page)
        self.assertIn('Jaccard matrix',page)
        self.assertIn('intersection=0; new=1; lost=1; union=2',page)

if __name__=='__main__':unittest.main()
