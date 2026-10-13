# CellRanger `count` under mrp and under the generated Nextflow

An opt-in local check against a real production pipeline: 10x Genomics
CellRanger's `cellranger testrun`, the `count` pipeline on its bundled tiny
dataset, run by `mrp` and by the transpiled Nextflow project, with
`metrics_summary.csv` compared byte for byte. CellRanger is licensed software
and is not in this repository, so the check runs only where a bundle is
extracted, never in CI.

```sh
CELLRANGER_HOME=~/Downloads/cellranger-10.1.0 make test-cellranger
```

`TestCellRangerDifferential` (`test/e2e/cellranger_test.go`) skips unless
`CELLRANGER_HOME` points at an extracted bundle holding `bin/cellranger`,
`sourceme.bash`, `mro` and the bundled Martian runtime. With the bundle's
environment sourced, it:

1. writes the `count` invocation `.mro` with `cellranger testrun --dry`;
2. runs it under the bundle's `mrp` (`--jobmode=local --nopreflight`), sized
   by `CELLRANGER_LOCALCORES` and `CELLRANGER_LOCALMEM` (default 4 cores,
   8 GB);
3. transpiles the same `.mro` with `-mropath <bundle>/mro`;
4. runs the project on the `standard` profile, with the stages under the
   bundle's own `mrjob` and adapters (`--mro2nf_mrjob`, `--mro2nf_adapters`),
   and with `process.resourceLimits` set to the same cores and memory. Each
   job is granted from that pool and its task clamped to it, so the 30 GB
   cloupe join runs on a smaller machine, as `mrp` grants it;
5. requires `metrics_summary.csv` to be byte-identical, and the main outputs
   (`filtered_feature_bc_matrix.h5`, `raw_feature_bc_matrix.h5`,
   `cloupe.cloupe`, `web_summary.html`, `possorted_genome_bam.bam`,
   `molecule_info.h5`) to be published;
6. logs the task counts from the trace.

With CellRanger 10.1.0 the test passes: `metrics_summary.csv` is
byte-identical to `mrp`'s, and the Nextflow run has 110 tasks, 108 stage
tasks plus PLAN and OUTS. It runs the pipeline twice; on the machine it was
last run on it took about 8 minutes. `make test-e2e` excludes it.
