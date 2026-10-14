#!/usr/bin/env python3
"""Classify what each task stages in, from the S3 downloads its .command.run lists, and size them.

  inter     files another task wrote: (producer task ID, path in its work dir, staged name)
  ctl       the task's own control files (.command.sh, .command.run)
  foreign   files Nextflow uploaded from the launch host (<work dir>/stage-*)
  other     any other S3 source, such as an input staged from outside the work dir
  producers the task IDs the task stages files of: its file dependencies
With s3ls.txt, bytes_in sums the sizes of what it stages and bytes_out the outputs it wrote.

usage: stagein.py RUN_DIR   (reads tasks.json, runs/<task dir>/.command.run, s3ls.txt if present; writes stagein.json)
"""
import argparse
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import perfrun

DOWNLOAD = re.compile(r'downloads\+=\("nxf_s3_download (\S+) (\S+)"\)')
OUTPUTS = ('outs', 'chunks', 'chunkout', 'forks', 'chunk_refs', 'results', 'pipeline_outs.json')


def prefix_sizes(run_dir):
    """Bytes under every key prefix of the work-dir listing."""
    pref = {}
    for k, _, n in perfrun.s3_listing(run_dir):
        parts = k.split('/')
        for i in range(1, len(parts) + 1):
            kk = '/'.join(parts[:i])
            pref[kk] = pref.get(kk, 0) + n
    return pref


def classify(run, rel, root, byrel):
    dl = DOWNLOAD.findall(run)
    pat = re.compile(rf'{re.escape(root)}/({perfrun.HASH_DIR})/(.*)')
    inter, foreign, ctl, other = [], [], [], []
    for src, dst in dl:
        m = pat.match(src)
        if m and m.group(1) == rel:
            ctl.append(dst)
        elif m:
            p = byrel.get(m.group(1))
            inter.append((p['task_id'] if p else '?', m.group(2), dst))
        elif src.startswith(f'{root}/stage-'):
            foreign.append(src.split('/')[-1])
        else:
            other.append(src)
    return dl, dict(inter=inter, foreign=foreign, ctl=ctl, other=other, globs=re.findall(r'eval "ls -1d ([^"]+)"', run),
                    producers=sorted({i[0] for i in inter}, key=lambda x: int(x) if x != '?' else 0))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument('run_dir')
    a = ap.parse_args()
    tasks = perfrun.read_json(a.run_dir, 'tasks.json')
    root = perfrun.work_dir(tasks)
    byrel = {perfrun.rel_dir(root, t['workdir']): t for t in tasks}
    pref = prefix_sizes(a.run_dir)

    def size_of(src):
        return pref.get(perfrun.s3_key(src).rstrip('/'), 0)

    res = {}
    for t in tasks:
        rel = perfrun.rel_dir(root, t['workdir'])
        with open(os.path.join(a.run_dir, 'runs', rel, '.command.run')) as f:
            dl, r = classify(f.read(), rel, root, byrel)
        if pref:
            staged = [(src, d) for src, d in dl if f'/{rel}/' not in src]
            r['bytes_in'] = sum(size_of(src) for src, _ in staged)
            r['bytes_in_by'] = sorted(((size_of(src), d) for src, d in staged), reverse=True)
            r['bytes_out'] = sum(pref.get(f'{perfrun.s3_key(root)}/{rel}/{d}', 0) for d in OUTPUTS)
        res[t['task_id']] = r
    perfrun.write_json(a.run_dir, 'stagein.json', res, indent=1)
    names = {t['task_id']: t['short'] for t in tasks}
    for tid in sorted(res, key=int):
        r = res[tid]
        print(f"{tid:>4} n_inter={len(r['inter']):3d} foreign={r['foreign']} other={len(r['other'])} globs={r['globs']} "
              f"producers={r['producers']} {names[tid]}")


if __name__ == '__main__':
    main()
