#!/usr/bin/env python3
"""Compare decoder detail levels on an explicit synthetic metadata fixture."""
import argparse
import json
from pathlib import Path
from test_detail_levels import synthetic_benchmark


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output',required=True,type=Path)
    parser.add_argument('--repeats',type=int,default=11)
    parser.add_argument('--processes',type=int,default=8)
    parser.add_argument('--pages-per-category',type=int,default=16)
    parser.add_argument('--prefetch-pages',type=int,default=1)
    parser.add_argument('--resident-sample-pages',type=int,default=0,
                        help='zero disables sampling; positive values use a synthetic prefix census, never native sampling cost')
    args=parser.parse_args()
    result=synthetic_benchmark(args.repeats,processes=args.processes,pages_per_category=args.pages_per_category,
                               prefetch_pages=args.prefetch_pages,resident_sample_pages=args.resident_sample_pages)
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.write_text(json.dumps(result,indent=2)+'\n')


if __name__=='__main__':main()
