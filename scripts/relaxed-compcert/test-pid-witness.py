"""Exercise the same Linux pidfd witness on a harmless sleep process in a temporary directory."""
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import time
s=importlib.util.spec_from_file_location('pid_witness',Path(__file__).with_name('pid-witness.py'))
m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
with tempfile.TemporaryDirectory() as parent:
    m.PATH=Path(parent)/'evidence';m.EXPECTED=['sleep','0.4']
    p=subprocess.Popen(m.EXPECTED)
    armed=m.main('arm',p.pid)
    assert m.main('before_signal_zero',p.pid)['current']['start_ticks']==armed['start_ticks']
    p.wait()
    for _ in range(100):
        if (m.PATH/'terminal').exists():break
        time.sleep(.01)
    closed=m.main('closed',p.pid)
    assert closed['terminal']['observed_exit_monotonic_ns']>=armed['armed_monotonic_ns']
    assert m.main('source_final',p.pid)['terminal']==closed['terminal']
    print('Unique process identity, real pidfd exit and source-final continuity verified.')
