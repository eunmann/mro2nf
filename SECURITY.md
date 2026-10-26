# Security

## What runs with whose rights

mro2nf runs your pipeline's stage code. It adds no sandbox: a stage can do
whatever the identity it runs as can do.

| Profile | Stage code runs as |
|---|---|
| `standard`, grid | the user who launched Nextflow, or the scheduler's job user |
| `docker` | the invoking user's uid inside the container, with access to the task's staged files |
| `awsbatch` | the image's user, root for the runtime image, with the job role (`aws.batch.jobRole`) or, without one, the instance role |
| `healthomics` | the image's user, with the run's IAM role |

- Run only pipelines and images you trust, and give the AWS roles only the
  buckets and services the pipeline needs. The example stack in
  `deploy/awsbatch-cdk` lets its instances read only the buckets it mounts.
- A run reads file inputs by absolute path or `s3://` URI. Other URI schemes
  are refused unless `--mro2nf_allow_remote_inputs true`.
- The container profiles fail a stage whose output resolves outside its task,
  its inputs and the pipeline's code (`--mro2nf_confine`). On the other
  profiles such an output is copied in and published, as `mrp` publishes it.
- A HealthOmics workflow pins its image by digest, and its parameter template
  lets a run set only the pipeline's inputs and `mro2nf_overrides`.

## Reporting a vulnerability

Report it privately through GitHub's
[private vulnerability reporting](https://github.com/eunmann/mro2nf/security/advisories/new).
Do not open a public issue. Include the version (`mro2nf -version`), the
profile, and the steps to reproduce.
