#!/usr/bin/env node
import * as cdk from 'aws-cdk-lib';
import { Mro2nfStack } from '../lib/stack';

const app = new cdk.App();

// The input buckets every Batch instance mounts besides the work bucket:
// `npx cdk deploy -c mountBuckets=bucket-a,bucket-b`.
const mountBuckets = String(app.node.tryGetContext('mountBuckets') ?? '')
  .split(',')
  .map((b) => b.trim())
  .filter((b) => b !== '');

// Environment-agnostic: deploys to whatever account/region your AWS CLI is
// configured for, and synthesizes offline (the VPC uses Fn::GetAZs rather than an
// account-specific availability-zone lookup, so `cdk synth` works before creds).
// The root volume's provisioned gp3 throughput and IOPS, gp3's maxima unless
// set: `-c rootThroughputMiBps=1000 -c rootIops=4000`.
const contextNumber = (key: string): number | undefined => {
  const v = app.node.tryGetContext(key);
  return v === undefined ? undefined : Number(v);
};

new Mro2nfStack(app, 'Mro2nfStack', {
  mountBuckets,
  rootThroughputMiBps: contextNumber('rootThroughputMiBps'),
  rootIops: contextNumber('rootIops'),
});

app.synth();
