#!/usr/bin/env node
import * as cdk from 'aws-cdk-lib';
import { Mro2nfStack } from '../lib/stack';

const app = new cdk.App();

// Environment-agnostic: deploys to whatever account/region your AWS CLI is
// configured for, and synthesizes offline (the VPC uses Fn::GetAZs rather than an
// account-specific availability-zone lookup, so `cdk synth` works before creds).
new Mro2nfStack(app, 'Mro2nfStack', {});

app.synth();
