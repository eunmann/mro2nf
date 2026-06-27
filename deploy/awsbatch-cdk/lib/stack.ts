import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as ecr from 'aws-cdk-lib/aws-ecr';
import * as s3 from 'aws-cdk-lib/aws-s3';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as batch from 'aws-cdk-lib/aws-batch';

/**
 * Minimal infrastructure to run a transpiled Martian->Nextflow pipeline live on
 * AWS, for BOTH supported cloud targets:
 *
 *   -target awsbatch  : the Batch compute environment + queue + S3 work dir.
 *   -target healthomics: the ECR repo + S3 + an Omics service role (you register
 *                        the workflow zip with `aws omics create-workflow`).
 *
 * Shared by both: one S3 bucket (work dir + outputs) and one ECR repo (the
 * runtime image built from the generated Dockerfile). Everything is tagged for
 * easy teardown — `cdk destroy` removes it all. This is an example, not a
 * hardened production deployment.
 */
export class MartNextflowStack extends cdk.Stack {
  constructor(scope: Construct, id: string, props?: cdk.StackProps) {
    super(scope, id, props);

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
      // Cost guards: expire the (intermediate) Nextflow work dir, drop old
      // HealthOmics scratch, and clean up orphaned multipart-upload parts so
      // storage can't silently accumulate. results/ is left for you to retrieve.
      lifecycleRules: [
        { prefix: 'work/', expiration: cdk.Duration.days(14) },
        { prefix: 'omics-out/', expiration: cdk.Duration.days(14) },
        { abortIncompleteMultipartUploadAfter: cdk.Duration.days(3) },
      ],
    });

    // ECR repo for the runtime image (built from the generated Dockerfile).
    // Keep a bounded number of recent images so layers can't pile up (room for
    // version history and a multi-pipeline test matrix; they share base layers).
    const repo = new ecr.Repository(this, 'RuntimeRepo', {
      repositoryName: 'mart-runtime',
      removalPolicy: cdk.RemovalPolicy.DESTROY,
      emptyOnDelete: true,
      lifecycleRules: [{ maxImageCount: 20 }],
    });

    // --- AWS Batch (-target awsbatch) ---

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

    const computeEnv = new batch.ManagedEc2EcsComputeEnvironment(this, 'ComputeEnv', {
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PUBLIC },
      instanceRole,
      useOptimalInstanceClasses: true,
      // Cost: spot (~70% cheaper) and — critically — minvCpus 0 so Batch runs NO
      // instances while idle (it scales to zero between runs; you pay only for
      // the minutes a job actually runs). maxvCpus caps the worst-case concurrent
      // spend; a test pipeline uses 1–2 vCPUs.
      spot: true,
      minvCpus: 0,
      maxvCpus: 16,
    });

    const queue = new batch.JobQueue(this, 'JobQueue', {
      computeEnvironments: [{ computeEnvironment: computeEnv, order: 1 }],
    });

    // --- AWS HealthOmics (-target healthomics) ---

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

    // --- Outputs (feed these to `mart` / `nextflow run` / `aws omics`) ---
    new cdk.CfnOutput(this, 'Region', { value: this.region });
    new cdk.CfnOutput(this, 'WorkBucketName', { value: bucket.bucketName });
    new cdk.CfnOutput(this, 'EcrRepoUri', { value: repo.repositoryUri });
    new cdk.CfnOutput(this, 'BatchJobQueue', { value: queue.jobQueueName });
    new cdk.CfnOutput(this, 'OmicsRoleArn', { value: omicsRole.roleArn });
  }
}
