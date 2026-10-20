#!/usr/bin/env node
import { App } from 'aws-cdk-lib';
import { SandboxxingMicroVMStack } from '../lib/stack';

const app = new App();
// The stack names no account or region: it is deployed to those of the AWS
// profile in use, and the availability zones are chosen by CloudFormation at
// deploy time. So no lookup has to be cached in cdk.context.json, which would
// hold the account ID of whoever deployed it.
new SandboxxingMicroVMStack(app, 'SandboxxingMicroVM', {
  description: 'AWS resources of the sandboxxing microvm backend',
});
