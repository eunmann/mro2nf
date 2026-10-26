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
