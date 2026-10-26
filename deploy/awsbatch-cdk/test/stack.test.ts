import { test } from 'node:test';
import * as assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import * as cdk from 'aws-cdk-lib';
import { Match, Template } from 'aws-cdk-lib/assertions';
import { Mro2nfStack } from '../lib/stack';

// The CLI synthesizes with cdk.json's context; a bare App would not, and its
// feature flags shape the template.
const { context } = JSON.parse(readFileSync(join(__dirname, '..', 'cdk.json'), 'utf8'));

const template = Template.fromStack(new Mro2nfStack(new cdk.App({ context }), 'Mro2nfStack'));

test('the root volume is gp3 at its provisioned throughput', () => {
  template.hasResourceProperties('AWS::EC2::LaunchTemplate', {
    LaunchTemplateData: {
      BlockDeviceMappings: [{ Ebs: Match.objectLike({ VolumeType: 'gp3', Throughput: 1000, Iops: 6000 }) }],
    },
  });
});

test('the root volume is encrypted', () => {
  template.hasResourceProperties('AWS::EC2::LaunchTemplate', {
    LaunchTemplateData: { BlockDeviceMappings: [{ Ebs: Match.objectLike({ Encrypted: true }) }] },
  });
});

test('the bucket is encrypted, private and TLS-only', () => {
  template.hasResourceProperties('AWS::S3::Bucket', {
    BucketEncryption: {
      ServerSideEncryptionConfiguration: [{ ServerSideEncryptionByDefault: { SSEAlgorithm: 'AES256' } }],
    },
    PublicAccessBlockConfiguration: {
      BlockPublicAcls: true,
      BlockPublicPolicy: true,
      IgnorePublicAcls: true,
      RestrictPublicBuckets: true,
    },
  });
  template.hasResourceProperties('AWS::S3::BucketPolicy', {
    PolicyDocument: {
      Statement: Match.arrayWith([Match.objectLike({
        Effect: 'Deny',
        Action: 's3:*',
        Condition: { Bool: { 'aws:SecureTransport': 'false' } },
      })]),
    },
  });
});

const thisAccount = { 'aws:SourceAccount': { Ref: 'AWS::AccountId' } };

test('HealthOmics assumes its role only for this account', () => {
  template.hasResourceProperties('AWS::IAM::Role', {
    AssumeRolePolicyDocument: {
      Statement: [Match.objectLike({
        Principal: { Service: 'omics.amazonaws.com' },
        Condition: {
          StringEquals: thisAccount,
          ArnLike: { 'aws:SourceArn': Match.anyValue() },
        },
      })],
    },
  });
});

test('HealthOmics pulls from the repository only for this account', () => {
  template.hasResourceProperties('AWS::ECR::Repository', {
    RepositoryPolicyText: {
      Statement: [Match.objectLike({
        Principal: { Service: 'omics.amazonaws.com' },
        Condition: { StringEquals: thisAccount },
      })],
    },
  });
});

interface Statement {
  Action: string | string[];
  Resource?: unknown;
}

const statements = (): Statement[] =>
  Object.values(template.findResources('AWS::IAM::Policy'))
    .flatMap((p) => p.Properties.PolicyDocument.Statement as Statement[]);

const actionsOf = (s: Statement): string[] => [s.Action].flat();

test('the log permissions name only the HealthOmics workflow log group', () => {
  const logs = statements().filter((s) => actionsOf(s).some((a) => a.startsWith('logs:')));
  assert.equal(logs.length, 1);
  assert.deepEqual(actionsOf(logs[0]).sort(),
    ['logs:CreateLogGroup', 'logs:CreateLogStream', 'logs:DescribeLogStreams', 'logs:PutLogEvents']);
  assert.match(JSON.stringify(logs[0].Resource), /:log-group:\/aws\/omics\/WorkflowLog:\*"/);
});

const jobRoles = () => template.findResources('AWS::IAM::Role', {
  Properties: {
    AssumeRolePolicyDocument: {
      Statement: [Match.objectLike({ Principal: { Service: 'ecs-tasks.amazonaws.com' } })],
    },
  },
});

test('Batch jobs run as a role only this account\'s ECS tasks assume', () => {
  const roles = Object.values(jobRoles());
  assert.equal(roles.length, 1);
  const [trust] = roles[0].Properties.AssumeRolePolicyDocument.Statement;
  assert.deepEqual(trust.Condition.StringEquals, thisAccount);
  assert.match(JSON.stringify(trust.Condition.ArnLike['aws:SourceArn']), /":ecs:"/);
  template.hasOutput('BatchJobRoleArn', { Value: { 'Fn::GetAtt': [Object.keys(jobRoles())[0], 'Arn'] } });
});

test('the job role writes only the expiring work dir and deletes nothing', () => {
  const [role] = Object.keys(jobRoles());
  const own = Object.values(template.findResources('AWS::IAM::Policy'))
    .filter((p) => JSON.stringify(p.Properties.Roles) === JSON.stringify([{ Ref: role }]))
    .flatMap((p) => p.Properties.PolicyDocument.Statement as Statement[]);
  const actions = own.flatMap(actionsOf);
  assert.ok(actions.includes('s3:PutObject'), `the job role cannot write: ${actions}`);
  assert.deepEqual(actions.filter((a) => a.startsWith('s3:Delete')), []);

  const [bucket] = Object.values(template.findResources('AWS::S3::Bucket'));
  const expiring = bucket.Properties.LifecycleConfiguration.Rules
    .filter((r: { ExpirationInDays?: number }) => r.ExpirationInDays !== undefined)
    .map((r: { Prefix: string }) => r.Prefix);
  assert.deepEqual(expiring, ['work/']);
  for (const s of own.filter((s) => actionsOf(s).includes('s3:PutObject'))) {
    assert.match(JSON.stringify(s.Resource), /^\{"Fn::Join":\["",\[\{"Fn::GetAtt":\["WorkBucket[0-9A-F]+","Arn"\]\},"\/work\/\*"\]\]\}$/);
  }
});

test('the queue cancels a job no instance can fit', () => {
  const queues = Object.values(template.findResources('AWS::Batch::JobQueue'));
  assert.equal(queues.length, 1);
  const actions = queues[0].Properties.JobStateTimeLimitActions;
  for (const a of actions) {
    assert.deepEqual([a.Action, a.State], ['CANCEL', 'RUNNABLE']);
  }
  assert.deepEqual(actions.map((a: { Reason: string }) => a.Reason).sort(), [
    'MISCONFIGURATION:COMPUTE_ENVIRONMENT_MAX_RESOURCE',
    'MISCONFIGURATION:JOB_RESOURCE_REQUIREMENT',
  ]);
});
