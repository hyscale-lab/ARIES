import json
import sys
from pathlib import Path

mode, stage = sys.argv[1:]
on = mode == 'on'
p = Path('/sys/module/damon_reclaim/parameters')
settings = dict(skip_anon='Y', min_age='100000', quota_ms='20', quota_sz='1073741824',
                quota_reset_interval_ms='500', wmarks_high='990', wmarks_mid='990',
                wmarks_low='200', wmarks_interval='1000000')
order = Path('/sys/module/page_reporting/parameters/page_reporting_order')
values = {f.name:f.read_text().strip() for f in p.iterdir() if f.name != 'commit_inputs'}
features = next(Path('/sys/bus/virtio/drivers/virtio_balloon').glob('virtio*/features')).read_text().strip()
print(json.dumps(dict(stage=stage, params=values, order=order.read_text().strip(), features=features,
                      reporting=features[5]=='1', page_size=__import__('os').sysconf('SC_PAGE_SIZE'),
                      swap=Path('/proc/swaps').read_text(), kernel=__import__('os').uname().release)))
assert (values['enabled']=='Y') == on and (features[5]=='1') == on
if on:
    assert order.read_text().strip()=='6' and all(values[k]==v for k,v in settings.items())
