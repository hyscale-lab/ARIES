"""Synthetic x86-64 metadata fixture; timings are local Python costs, not VM costs."""
import copy
import hashlib
import json
from pathlib import Path
import statistics
import sys
import tempfile
import unittest
from types import SimpleNamespace

from capture_memory import CaptureMemory
from decoder import GuestDecoder,attribution_detail
from replay import FrozenMemory

PAGE=4096
KERNEL=0xffff888000000000


class SyntheticGuest:
    def __init__(self,processes=8,pages_per_category=16):
        layouts=Path(__file__).parent/'layouts/linux-5.10.239'
        self.base=json.loads((layouts/'base-layout.json').read_text())
        self.owner=json.loads((layouts/'owner-layout.json').read_text())
        self.names=json.loads((layouts/'names-layout.json').read_text())
        self.pages={};self.next=1<<20;self.process_count=processes
        self.regions=[{'slot':0,'guest_base':0,'bytes':64<<20}]
        self.root=PAGE
        self.put(self.root+((KERNEL>>39)&511)*8,2*PAGE|3)
        self.put(2*PAGE+((KERNEL>>30)&511)*8,0x83)  # Kernel 1GiB direct map.
        self.targets=[(32<<20)+i*PAGE for i in range(6*pages_per_category)]
        self.page_array=KERNEL+(16<<20)
        o=self.names['offsets'];l=self.base['layout'];self.files=[];self.inodes=[];self.mappings=[];mounts=[]
        namespace=self.alloc();roots=[];dentries=[];superblocks=[]
        for index,filesystem in enumerate(['ext4','tmpfs']):
            superblock=self.alloc();fstype=self.alloc();name=self.alloc();self.write(name,filesystem.encode()+b'\0')
            self.put(superblock+o['super_block.s_type'],KERNEL+fstype)
            self.put(fstype+o['file_system_type.name'],KERNEL+name)
            self.put(superblock+o['super_block.s_dev'],index+1,4)
            self.write(superblock+o['super_block.s_id'],filesystem.encode()+b'\0')
            root=self.dentry('',None,superblock);inode=self.alloc();mapping=self.alloc()
            self.put(inode+l['inode']['i_mapping'],KERNEL+mapping)
            self.put(mapping+l['address_space']['host'],KERNEL+inode)
            self.put(inode+o['inode.i_sb'],KERNEL+superblock)
            self.put(inode+o['inode.i_ino'],index+100)
            self.put(inode+o['inode.i_mode'],0o100644,2);self.put(inode+o['inode.i_nlink'],1,4)
            dent=self.dentry('first.bin' if index==0 else 'second.bin',root,superblock,inode)
            self.put(inode+o['inode.i_dentry'],KERNEL+dent+o['dentry.d_u.d_alias'])
            mount=self.alloc();file=self.alloc()
            self.put(file+l['file']['f_path']+l['path']['dentry'],KERNEL+dent)
            self.put(file+l['file']['f_path']+l['path']['mnt'],KERNEL+mount+o['mount.mnt'])
            self.files.append(file);self.inodes.append(inode);self.mappings.append(mapping)
            mounts.append(mount);roots.append(root);dentries.append(dent);superblocks.append(superblock)
        mountpoint=self.dentry('shm',roots[0],superblocks[0])
        head=KERNEL+namespace+o['mnt_namespace.list']
        self.put(namespace+o['mnt_namespace.root'],KERNEL+mounts[0]);self.put(namespace+o['mnt_namespace.mounts'],2,4)
        self.put(namespace+o['mnt_namespace.ns.inum'],123,4)
        self.put(namespace+o['mnt_namespace.list'],KERNEL+mounts[0]+o['mount.mnt_list'])
        for i,mount in enumerate(mounts):
            self.put(mount+o['mount.mnt_list'],KERNEL+mounts[1]+o['mount.mnt_list'] if i==0 else head)
            self.put(mount+o['mount.mnt_parent'],KERNEL+mounts[0])
            self.put(mount+o['mount.mnt_mountpoint'],KERNEL+(roots[0] if i==0 else mountpoint))
            self.put(mount+o['mount.mnt']+o['vfsmount.mnt_root'],KERNEL+roots[i])
            self.put(mount+o['mount.mnt']+o['vfsmount.mnt_sb'],KERNEL+superblocks[i])
            self.put(mount+o['mount.mnt_ns'],KERNEL+namespace);self.put(mount+o['mount.mnt_id'],i+1,4)
        tasks=[]
        for i in range(processes):
            task=self.alloc(8192);mm=self.alloc();proxy=self.alloc();tasks.append(task)
            self.put(task+l['task_struct']['pid'],i+1,4);self.put(task+l['task_struct']['tgid'],i+1,4)
            self.write(task+l['task_struct']['comm'],('proc'+str(i)).encode()+b'\0')
            self.put(task+l['task_struct']['mm'],KERNEL+mm)
            self.put(task+self.owner['offsets']['task_struct.thread_group'],KERNEL+task+self.owner['offsets']['task_struct.thread_group'])
            self.put(task+o['task_struct.nsproxy'],KERNEL+proxy);self.put(proxy+o['nsproxy.mnt_ns'],KERNEL+namespace)
            self.put(task+self.owner['offsets']['task_struct.stack'],KERNEL+self.alloc(16384))
            pgd=self.alloc();l3=self.alloc();l2=self.alloc();l1=self.alloc()
            self.put(mm+l['mm_struct']['pgd'],KERNEL+pgd)
            self.put(pgd,l3|3);self.put(l3,l2|3);self.put(l2+2*8,l1|3)
            vmas=[self.alloc() for _ in range(3)]
            self.put(mm+l['mm_struct']['mmap'],KERNEL+vmas[0])
            for j,vma in enumerate(vmas):
                start=0x400000+j*pages_per_category*PAGE;end=start+pages_per_category*PAGE
                self.put(vma+l['vm_area_struct']['vm_start'],start);self.put(vma+l['vm_area_struct']['vm_end'],end)
                self.put(vma+l['vm_area_struct']['vm_next'],KERNEL+vmas[j+1] if j<2 else 0)
                if j:self.put(vma+l['vm_area_struct']['vm_file'],KERNEL+self.files[j-1])
                for page in range(j*pages_per_category,(j+1)*pages_per_category):self.put(l1+page*8,self.targets[page]|3)
        for i,task in enumerate(tasks):self.put(task+l['task_struct']['tasks'],KERNEL+tasks[(i+1)%len(tasks)]+l['task_struct']['tasks'])
        flag=l['pageflags']
        for i,gpa in enumerate(self.targets):
            group=i//pages_per_category;flags=(1<<flag['PG_dirty']) if i%4==0 else 0
            flags|=(1<<flag['PG_writeback']) if i%9==0 else 0
            mapping=1 if group==0 else KERNEL+self.mappings[group-1] if group in (1,2) else 0
            if group==2:flags|=1<<flag['PG_swapbacked']
            if group==3:flags|=1<<flag['PG_slab']
            if group==4:flags|=1<<flag['PG_reserved']
            address=self.page_array-KERNEL+(gpa//PAGE)*l['page']['sizeof']
            for key,value,size in [('flags',flags,8),('mapping',mapping,8),('index',i,8),('_refcount',1,4),('_mapcount',0xfffffbff if group==5 else 0xffffffff,4)]:self.put(address+l['page'][key],value,size)
        node_array=self.alloc();node=self.alloc();self.put(node_array,KERNEL+node)
        self.put(node,1,4);self.put(node+8,KERNEL+node+8)  # One empty buddy list.
        info={'SYMBOL(node_data)':hex(KERNEL+node_array),'LENGTH(node_data)':'1','OFFSET(pglist_data.nr_zones)':'0',
              'OFFSET(pglist_data.node_zones)':'8','SIZE(zone)':'32','LENGTH(zone.free_area)':'1','OFFSET(zone.free_area)':'0',
              'SIZE(free_area)':'16','LENGTH(free_area.free_list)':'1','OFFSET(free_area.free_list)':'0','OFFSET(page.lru)':'8'}
        self.anchor={'kernel_sha256':self.base['kernel_sha256'],'kernel_pgd_phys':self.root,'vmemmap_base':self.page_array,
                     'kaslr_slide':0,'text_phys':32<<20,'vmcoreinfo':'\n'.join(k+'='+v for k,v in info.items())}
        self.base['symbols']={'init_task':KERNEL+tasks[0]}
        self.owner['symbols']={'_stext':KERNEL+(32<<20)};self.owner['allocated_sections']=[]
        self.reading={'vm_instance':'synthetic','tool_call_id':'same-source-epoch','accessed_gpas':self.targets,'unknown_gpas':{}}

    def alloc(self,size=PAGE):
        result=self.next;self.next+=(size+PAGE-1)//PAGE*PAGE;return result

    def write(self,address,data):
        for i,value in enumerate(data):self.pages.setdefault((address+i)&~4095,bytearray(PAGE))[(address+i)&4095]=value

    def put(self,address,value,size=8):self.write(address,value.to_bytes(size,'little'))

    def dentry(self,name,parent,superblock,inode=0):
        d=self.alloc();text=self.alloc();l=self.base['layout'];o=self.names['offsets'];self.write(text,name.encode())
        self.put(d+l['dentry']['d_parent'],KERNEL+(d if parent is None else parent))
        self.put(d+l['dentry']['d_name']+l['qstr']['len'],len(name),4)
        self.put(d+l['dentry']['d_name']+l['qstr']['name'],KERNEL+text)
        self.put(d+l['dentry']['d_inode'],KERNEL+inode if inode else 0)
        self.put(d+o['dentry.d_sb'],KERNEL+superblock);self.put(d+o['dentry.d_hash.pprev'],1)
        return d

    def read_pages(self,gpas):return {g:bytes(self.pages.get(g,bytearray(PAGE))) for g in gpas}

    def resident_sample(self,seed,requested):
        # Prefix selection is deliberate fixture data, not the native sampler.
        selection=[{'gpa':g,'host_pfn':i+1,'host_kpagecount':1,'host_kpageflags':0}
                   for i,g in enumerate(self.targets[:requested])]
        return {'selection':selection,'resident_population':{'unique_nonzero_host_pfns':len(self.targets),
                'resident_gpa_pages':len(self.targets)},'sample_design':{'sample_pages':len(selection),
                'population_pages':len(self.targets),'requested_pages':requested,'fixture_prefix_selection':True}}

    def decoder(self,detail='file',memory=None,*,prefetch_pages=1,resident_sample_pages=0):
        d=GuestDecoder.__new__(GuestDecoder);d.pid=0;d.vm_id='synthetic';d.slots=[]
        d.config={'owner_layout_data':copy.deepcopy(self.owner),'names_layout_data':copy.deepcopy(self.names)}
        if detail is not None:d.config['attribution_detail']=detail
        if resident_sample_pages:d.config.update(enable_resident_sampling=True,resident_sample_pages=resident_sample_pages)
        d.mem=memory if memory is not None else CaptureMemory(self,prefetch_pages=prefetch_pages)
        d.meta=copy.deepcopy(self.base);d.l=d.meta['layout'];d.a=dict(self.anchor);d.cache={};d.errors=[]
        d.image=SimpleNamespace(symbols=d.meta['symbols'])
        d.info=dict(line.split('=',1) for line in d.a['vmcoreinfo'].splitlines())
        return d


def synthetic_benchmark(repetitions=11,*,processes=8,pages_per_category=16,prefetch_pages=1,resident_sample_pages=0):
    if not 1<=repetitions<=1000 or not 1<=processes<=128 or not 1<=pages_per_category<=128 or not 1<=prefetch_pages<=256 or resident_sample_pages<0:
        raise ValueError('invalid bounded synthetic benchmark dimensions')
    guest=SyntheticGuest(processes,pages_per_category)
    result={'schema':'aries.attribution-detail-synthetic-benchmark.v1',
        'scope':'Synthetic in-memory provider and x86-64 page tables; local decoder cost only, no VMM/HTTP/pause cost',
        'selected_pages':len(guest.targets),'processes':guest.process_count,'same_reading':True,
        'prefetch_pages':prefetch_pages,'resident_sampling':resident_sample_pages>0,
        'resident_sample_pages_requested':resident_sample_pages,
        'resident_sampling_scope':'Synthetic census restricted to selected GPAs and deterministic prefix; native full-census export/sampling costs are not measured',
        'repetitions':repetitions,'levels':{}}
    for level in ['category','process','file']:
        runs=[guest.decoder(level,prefetch_pages=prefetch_pages,resident_sample_pages=resident_sample_pages).capture_metadata(guest.reading)['metrics'] for _ in range(repetitions)]
        result['levels'][level]={'decoder_closure_bytes':runs[-1]['decoder_closure_bytes'],
            'selected_gpas_sha256':runs[-1]['selected_gpas_sha256'],
            'sample_gpas_sha256':runs[-1]['sample_gpas_sha256'],'layout_identity':runs[-1]['layout_identity'],
            'category_coverage':runs[-1]['category_coverage'],
            'wall_ns':int(statistics.median(run['wall_ns'] for run in runs)),
            'cpu_ns':int(statistics.median(run['cpu_ns'] for run in runs)),
            'provider_totals':runs[-1]['provider_totals'],'stages':{name:{**value,
              'wall_ns':int(statistics.median(run['stages'][name]['wall_ns'] for run in runs)) if value['executed'] else None,
              'cpu_ns':int(statistics.median(run['stages'][name]['cpu_ns'] for run in runs)) if value['executed'] else None}
              for name,value in runs[-1]['stages'].items()}}
    identities={tuple(r[k] for k in ['selected_gpas_sha256','sample_gpas_sha256','layout_identity']) for r in result['levels'].values()}
    result['comparison_identity_matches']=len(identities)==1
    result['source_sha256']={name:hashlib.sha256(Path(__file__).with_name(name).read_bytes()).hexdigest()
                            for name in ['benchmark_detail.py','test_detail_levels.py','decoder.py','capture_memory.py','owners.py','file_names.py']}
    return result


class DetailTests(unittest.TestCase):
    def test_coarse_category_keeps_flags_but_does_not_claim_uncollected_owners_or_names(self):
        guest=SyntheticGuest();bundle=guest.decoder('category').capture_metadata(guest.reading)
        self.assertEqual(bundle['summary']['categories'],dict.fromkeys(['anonymous','file_cache','shmem','kernel_slab','reserved','kernel_page_table'],16))
        self.assertIsNone(bundle['summary']['process_count']);self.assertIsNone(bundle['summary']['pages_with_process_mapping'])
        self.assertEqual(bundle['tasks'],[]);self.assertIsNone(bundle['owners']);self.assertIsNone(bundle['names'])
        self.assertTrue(all(r['ownership_status']=='not_collected_detail_level' for r in bundle['rows']))
        self.assertEqual(sum(r['guest_page_dirty_flag'] for r in bundle['rows']),24)
        self.assertFalse(bundle['metrics']['stages']['process_mappings']['executed'])

    def test_process_and_file_levels_add_real_mappings_and_namespace_paths(self):
        guest=SyntheticGuest();process=guest.decoder('process').capture_metadata(guest.reading);full=guest.decoder('file').capture_metadata(guest.reading)
        self.assertEqual(process['summary']['process_count'],8);self.assertEqual(process['summary']['pages_with_process_mapping'],48)
        self.assertEqual(len(process['rows'][0]['process_owners']),8)
        self.assertFalse(any('file' in vma for vma in process['vmas']));self.assertIsNone(process['names'])
        self.assertEqual({path['path'] for file in full['files'].values() for path in file['namespace_paths']},{'/first.bin','/shm/second.bin'})
        self.assertFalse(full['summary']['errors']);self.assertFalse(full['summary']['file_namespace_errors'])
        self.assertLess(process['metrics']['decoder_closure_bytes'],full['metrics']['decoder_closure_bytes'])
        default=guest.decoder(None).capture_metadata(guest.reading)
        self.assertEqual(default['rows'],full['rows']);self.assertEqual(default['vmas'],full['vmas']);self.assertEqual(default['files'],full['files'])

    def test_every_detail_level_replays_from_its_retained_metadata_closure(self):
        guest=SyntheticGuest()
        with tempfile.TemporaryDirectory() as temporary:
            for detail in ['category','process','file']:
                decoder=guest.decoder(detail);bundle=decoder.capture_metadata(guest.reading);output=Path(temporary)/detail
                decoder.publish_capture(bundle,output)
                frozen=FrozenMemory(output/'metadata-index.json',output/'metadata-pages.bin.gz')
                replay=guest.decoder(detail,memory=frozen).capture_metadata(guest.reading)
                self.assertEqual(replay['rows'],bundle['rows']);self.assertEqual(replay['vmas'],bundle['vmas']);self.assertEqual(replay['files'],bundle['files'])
                self.assertEqual(replay['metrics']['decoder_closure_bytes'],bundle['metrics']['decoder_closure_bytes'])

    def test_uncollected_free_tails_and_unknown_accesses_remain_explicit(self):
        guest=SyntheticGuest();l=guest.base['layout'];gpa=guest.targets[0]
        address=guest.page_array-KERNEL+gpa//PAGE*l['page']['sizeof']
        guest.put(address+l['page']['_mapcount'],0xffffff7f,4)
        reading={**guest.reading,'accessed_gpas':guest.targets[1:],'unknown_gpas':{str(gpa):'not-on-lru'}}
        bundle=guest.decoder('category').capture_metadata(reading)
        self.assertEqual(bundle['rows'][0]['category'],'free_buddy_head')
        self.assertEqual(bundle['rows'][0]['access_unknown_reason'],'not-on-lru')
        self.assertEqual(bundle['summary']['detail_scope']['free_pages'],'buddy_heads_only')
        with self.assertRaises(ValueError):attribution_detail({'attribution_detail':'guess'})

    def test_category_bootstrap_still_validates_process_vma_layout(self):
        guest=SyntheticGuest();decoder=guest.decoder('category');l=decoder.l
        info={**decoder.info,'KERNELOFFSET':'0'}
        for field in ['flags','mapping','_refcount','_mapcount','private','compound_head']:
            info['OFFSET(page.'+field+')']=str(l['page'][field])
        for name in ['page','list_head','mem_section']:info['SIZE('+name+')']=str(l[name]['sizeof'])
        decoder.a['vmcoreinfo']='\n'.join(k+'='+v for k,v in info.items())
        decoder.image.resolve=lambda memory:dict(decoder.a)
        result=decoder.bootstrap()
        self.assertEqual(result['validated_process_count'],8);self.assertEqual(result['validated_vma_count'],24)
        self.assertFalse(decoder.errors)

    def test_failed_decoder_stage_retains_partial_read_counters(self):
        guest=SyntheticGuest();decoder=guest.decoder('process')
        # Corrupt the task list after the first task, rather than guessing zero tasks.
        first=guest.base['symbols']['init_task']-KERNEL
        guest.put(first+guest.base['layout']['task_struct']['tasks'],0)
        with self.assertRaises(ValueError):decoder.capture_metadata(guest.reading)
        self.assertFalse(decoder.decode_metrics['stages']['process_mappings']['completed'])
        self.assertGreater(decoder.decode_metrics['stages']['process_mappings']['provider_after']['consumed_pages'],0)
        self.assertEqual(decoder.decode_metrics['selected_pages'],96)

    def test_benchmark_controls_sampling_and_verifies_comparison_identity(self):
        result=synthetic_benchmark(2,processes=2,pages_per_category=2,resident_sample_pages=3,prefetch_pages=4)
        self.assertTrue(result['comparison_identity_matches']);self.assertTrue(result['resident_sampling'])
        self.assertEqual(result['resident_sample_pages_requested'],3);self.assertEqual(result['prefetch_pages'],4)


if __name__=='__main__':
    if '--benchmark' in sys.argv:print(json.dumps(synthetic_benchmark(),indent=2))
    else:unittest.main()
