#!/usr/bin/env node
import { App } from 'aws-cdk-lib';
import { SandboxxingMicroVMStack } from '../lib/stack';

const app = new App();
new SandboxxingMicroVMStack(app, 'SandboxxingMicroVM', {
  env: {
    account: process.env.CDK_DEFAULT_ACCOUNT,
    region: process.env.CDK_DEFAULT_REGION,
  },
  description: 'AWS resources of the sandboxxing microvm backend',
});
