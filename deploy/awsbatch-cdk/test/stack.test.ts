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
