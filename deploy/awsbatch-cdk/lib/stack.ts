import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecr from 'aws-cdk-lib/aws-ecr';
import * as s3 from 'aws-cdk-lib/aws-s3';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as batch from 'aws-cdk-lib/aws-batch';

/**
 * Where every Batch instance mounts the input buckets read-only, one dir per
 * bucket: the generated awsbatch profile's params.mro2nf_s3_mount, which
 * hands a stage an s3://<bucket>/<key> input as <root>/<bucket>/<key>.
 */
export const S3_MOUNT_ROOT = '/mnt/s3';

export interface Mro2nfStackProps extends cdk.StackProps {
  /**
   * Input buckets every Batch instance mounts besides the work bucket, which
   * it always mounts: a run reads an s3:// entry input of a mounted bucket in
   * place. The instance role may read only these and the work bucket, so a
   * run staging an input of any other bucket fails to download it.
   */
  readonly mountBuckets?: string[];
  /** The local read cache each mounted bucket may fill on an instance, in MiB. */
  readonly mountCacheMiB?: number;
  /** The gp3 root volume's provisioned throughput, MiB/s (125–2000). */
  readonly rootThroughputMiBps?: number;
  /** The gp3 root volume's provisioned IOPS (3000–80000). */
  readonly rootIops?: number;
}

// Nextflow copies every Batch task input to the root volume before the task
// starts and uploads its outputs after, at gp3's default 125 MiB/s unless
// provisioned higher. Measured on CellRanger, staging and Mountpoint's cache
// fills write ~200 KiB per op and peak just above 4,000 IOPS, so 6,000 keeps
// IOPS from capping the volume below its 1,000 MiB/s.
const ROOT_THROUGHPUT_MIBPS = 1000;
const ROOT_IOPS = 6000;

// An S3 bucket name, which the user data names unquoted.
const BUCKET_NAME = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/;

/**
 * The Mountpoint for S3 release every instance installs, and the SHA-256 of
 * its rpm per architecture, taken from rpms whose signatures verified
 * against the key upstream's INSTALL.md lists (8AEF E705 EBE3 29C0 948C
 * 75A6 6F1C 3B3A EF4B 030B). Every instance of a deployment then serves the
 * mount with the same binary.
 */
const MOUNTPOINT_VERSION = '1.24.0';
const MOUNTPOINT_SHA256 = {
  x86_64: 'fc3587c551dd2af2b502a57abea2eeb7b46ecc2bc0077f1d412827f521e88ebe',
  arm64: '3636465c56908c7f26182d6f31aaa77e4e145330833863104f4cca0db1788343',
};

/**
 * The boot script that installs Mountpoint for S3 and mounts each bucket
 * read-only at S3_MOUNT_ROOT/<bucket>, readable by every container user
 * (--allow-other), caching what tasks read on the root volume.
 */
function mountCommands(buckets: string[], cacheMiB: number): string[] {
  return [
    'set -euo pipefail',
    `case "$(uname -m)" in x86_64) a=x86_64 sum=${MOUNTPOINT_SHA256.x86_64};; aarch64) a=arm64 sum=${MOUNTPOINT_SHA256.arm64};; *) exit 1;; esac`,
    `curl -fsSL --retry 5 -o /tmp/mount-s3.rpm "https://s3.amazonaws.com/mountpoint-s3-release/${MOUNTPOINT_VERSION}/$a/mount-s3-${MOUNTPOINT_VERSION}-$a.rpm"`,
    'echo "$sum  /tmp/mount-s3.rpm" | sha256sum -c -',
    'dnf install -y /tmp/mount-s3.rpm',
    ...buckets.flatMap((b) => [
      `mkdir -p ${S3_MOUNT_ROOT}/${b} /var/cache/mount-s3/${b}`,
      `mount-s3 --read-only --allow-other --cache /var/cache/mount-s3/${b} --max-cache-size ${cacheMiB} ${b} ${S3_MOUNT_ROOT}/${b}`,
    ]),
  ];
}

/**
 * Minimal infrastructure to run a transpiled Martian->Nextflow pipeline live on
 * AWS, for BOTH supported cloud profiles of the one generated project:
 *
 *   -profile awsbatch : the Batch compute environment + queue + S3 work dir.
 *   healthomics       : the ECR repo + S3 + an Omics service role (you register
 *                       the package.sh zip with `aws omics create-workflow`).
 *
 * Shared by both: one S3 bucket (work dir + outputs) and one ECR repo (the
 * runtime image built from deploy/runtime/Dockerfile and the pipeline images
 * built FROM it, each the one image a run names). Everything is tagged for
 * easy teardown — `cdk destroy` removes it all. This is an example, not a
 * hardened production deployment.
 */
export class Mro2nfStack extends cdk.Stack {
  constructor(scope: Construct, id: string, props: Mro2nfStackProps = {}) {
    super(scope, id, props);

    const inputBuckets = props.mountBuckets ?? [];
    for (const b of inputBuckets) {
      if (!BUCKET_NAME.test(b)) {
        throw new Error(`mountBuckets: ${JSON.stringify(b)} is not an S3 bucket name`);
      }
    }

    // A small VPC with public subnets only (no NAT cost). Batch instances get
    // public IPs to pull from ECR; an S3 gateway endpoint keeps work-dir traffic
    // off the internet (cheaper + faster, and required for S3 in a private setup).
    const vpc = new ec2.Vpc(this, 'Vpc', {
      maxAzs: 2,
      natGateways: 0,
      subnetConfiguration: [{ name: 'public', subnetType: ec2.SubnetType.PUBLIC }],
      gatewayEndpoints: { S3: { service: ec2.GatewayVpcEndpointAwsService.S3 } },
    });

    // S3 work dir + outputs. autoDeleteObjects makes `cdk destroy` clean.
    const bucket = new s3.Bucket(this, 'WorkBucket', {
      removalPolicy: cdk.RemovalPolicy.DESTROY,
      autoDeleteObjects: true,
      // Cost guards: expire the intermediate Nextflow work dir and orphaned
      // multipart-upload parts. Final outputs never expire: results/ (the
      // awsbatch -output-dir) and omics-out/ (the HealthOmics --output-uri,
      // which receives each run's outputs below <run-id>/, and its logs).
      lifecycleRules: [
        { prefix: 'work/', expiration: cdk.Duration.days(14) },
        { abortIncompleteMultipartUploadAfter: cdk.Duration.days(3) },
      ],
    });

    // ECR repo for the runtime image (deploy/runtime/Dockerfile, tagged
    // runtime*) and the pipeline images built FROM it. Keep a bounded number
    // of recent images so layers can't pile up (room for version history and
    // a multi-pipeline test matrix; they share base layers). The runtime
    // images get their own rule, which ECR evaluates first, so pipeline
    // pushes never expire the image they build FROM.
    const repo = new ecr.Repository(this, 'RuntimeRepo', {
      repositoryName: 'mro2nf-runtime',
      removalPolicy: cdk.RemovalPolicy.DESTROY,
      emptyOnDelete: true,
      lifecycleRules: [
        { rulePriority: 1, tagPrefixList: ['runtime'], maxImageCount: 5 },
        { rulePriority: 2, maxImageCount: 20 },
      ],
    });

    // --- AWS Batch (-profile awsbatch) ---

    // The EC2 instances Batch launches need to pull the image (ECR) and stage
    // files to/from S3 (the aws CLI baked into the image uses this role).
    const instanceRole = new iam.Role(this, 'BatchInstanceRole', {
      assumedBy: new iam.ServicePrincipal('ec2.amazonaws.com'),
      managedPolicies: [
        iam.ManagedPolicy.fromAwsManagedPolicyName('service-role/AmazonEC2ContainerServiceforEC2Role'),
        iam.ManagedPolicy.fromAwsManagedPolicyName('AmazonEC2ContainerRegistryReadOnly'),
      ],
    });
    bucket.grantReadWrite(instanceRole);
    for (const b of inputBuckets) {
      s3.Bucket.fromBucketName(this, `MountBucket-${b}`, b).grantRead(instanceRole);
    }

    // Mountpoint for S3 mounts the work bucket and the input buckets on every
    // instance at boot, so a stage reads an s3:// input in place at one path
    // in every task, as mrp hands it the path it was given. Batch requires
    // MIME multipart user data in a launch template.
    const mountScript = ec2.UserData.forLinux();
    mountScript.addCommands(...mountCommands([bucket.bucketName, ...inputBuckets], props.mountCacheMiB ?? 102400));
    const userData = new ec2.MultipartUserData();
    userData.addUserDataPart(mountScript, ec2.MultipartBody.SHELL_SCRIPT, true);

    // Every task downloads the work-dir files it stages onto the instance's
    // root volume, where Mountpoint also caches each mounted bucket's reads,
    // and neither Martian nor Batch on EC2 sizes disk: Batch picks an
    // instance by vCPU and memory, and the AMI's 30 GiB root fills once a few
    // tasks on one instance stage large files. The volume is deleted with the
    // instance, so it costs nothing while idle.
    const launchTemplate = new ec2.LaunchTemplate(this, 'BatchLaunchTemplate', {
      requireImdsv2: true,
      userData,
      blockDevices: [{
        deviceName: '/dev/xvda',
        volume: ec2.BlockDeviceVolume.ebs(500, {
          volumeType: ec2.EbsDeviceVolumeType.GP3,
          throughput: props.rootThroughputMiBps ?? ROOT_THROUGHPUT_MIBPS,
          iops: props.rootIops ?? ROOT_IOPS,
        }),
      }],
    });

    const computeEnv = new batch.ManagedEc2EcsComputeEnvironment(this, 'ComputeEnv', {
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PUBLIC },
      instanceRole,
      launchTemplate,
      // The user data installs Mountpoint with dnf, which Amazon Linux 2023
      // has and the default ECS AMI, Amazon Linux 2, does not.
      images: [{ imageType: batch.EcsMachineImageType.ECS_AL2023 }],
      useOptimalInstanceClasses: true,
      // Cost: spot (~70% cheaper) and — critically — minvCpus 0 so Batch runs NO
      // instances while idle (it scales to zero between runs; you pay only for
      // the minutes a job actually runs). maxvCpus caps the worst-case concurrent
      // spend; a test pipeline uses 1–2 vCPUs. Raised to 256 so a parallel test
      // campaign (many fixtures + split chunks at once) is not vCPU-starved; idle
      // cost is unchanged (scales to zero between runs).
      spot: true,
      minvCpus: 0,
      maxvCpus: 256,
    });

    // Batch resolves the launch template version only when the compute
    // environment is created or updated; naming the latest version makes a
    // user-data change update the compute environment, and so its instances.
    (computeEnv.node.defaultChild as batch.CfnComputeEnvironment).addPropertyOverride(
      'ComputeResources.LaunchTemplate.Version', launchTemplate.latestVersionNumber);

    // A job no instance of the compute environment can ever fit stays RUNNABLE
    // at the head of the queue and holds back every job behind it, so the run
    // hangs instead of failing. Cancelling it after Batch's 10-minute minimum
    // fails that task, and so the run, loudly. A Spot capacity shortage is
    // transient and is left to wait.
    const queue = new batch.JobQueue(this, 'JobQueue', {
      computeEnvironments: [{ computeEnvironment: computeEnv, order: 1 }],
      jobStateTimeLimitActions: [
        batch.JobStateTimeLimitActionsReason.JOB_RESOURCE_REQUIREMENT,
        batch.JobStateTimeLimitActionsReason.COMPUTE_ENVIRONMENT_MAX_RESOURCE,
      ].map(reason => ({
        action: batch.JobStateTimeLimitActionsAction.CANCEL,
        maxTime: cdk.Duration.minutes(10),
        reason,
        state: batch.JobStateTimeLimitActionsState.RUNNABLE,
      })),
    });

    // --- AWS HealthOmics (the healthomics profile, package.sh) ---

    // The role HealthOmics assumes to read inputs / write outputs to S3 and pull
    // the image from ECR. Pass its ARN as --role-arn to `aws omics start-run`.
    const omicsRole = new iam.Role(this, 'OmicsServiceRole', {
      assumedBy: new iam.ServicePrincipal('omics.amazonaws.com'),
    });
    bucket.grantReadWrite(omicsRole);
    repo.grantPull(omicsRole);
    omicsRole.addToPolicy(new iam.PolicyStatement({
      actions: ['logs:CreateLogGroup', 'logs:CreateLogStream', 'logs:PutLogEvents', 'logs:DescribeLogStreams'],
      resources: ['*'],
    }));

    // HealthOmics pulls images via its service principal; allow it on this repo.
    repo.addToResourcePolicy(new iam.PolicyStatement({
      principals: [new iam.ServicePrincipal('omics.amazonaws.com')],
      actions: ['ecr:GetDownloadUrlForLayer', 'ecr:BatchGetImage', 'ecr:BatchCheckLayerAvailability'],
    }));

    // --- Outputs (feed these to `mro2nf` / `nextflow run` / `aws omics`) ---
    new cdk.CfnOutput(this, 'Region', { value: this.region });
    new cdk.CfnOutput(this, 'WorkBucketName', { value: bucket.bucketName });
    new cdk.CfnOutput(this, 'EcrRepoUri', { value: repo.repositoryUri });
    new cdk.CfnOutput(this, 'BatchJobQueue', { value: queue.jobQueueName });
    new cdk.CfnOutput(this, 'OmicsRoleArn', { value: omicsRole.roleArn });
  }
}
