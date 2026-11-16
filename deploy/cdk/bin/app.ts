#!/usr/bin/env node
import { App } from 'aws-cdk-lib';
import { SandboxxingEC2Stack } from '../lib/ec2-stack';
import { SandboxxingMicroVMStack } from '../lib/stack';

const app = new App();
// The stacks name no account or region: they are deployed to those of the AWS
// profile in use, and the availability zones are chosen by CloudFormation at
// deploy time. Only the EC2 stack's default VPC lookup needs both; it is
// cached in cdk.context.json, which is kept out of the repository because it
// holds the account ID of whoever deployed it.
switch (app.node.tryGetContext('backend') ?? 'microvm') {
  case 'microvm':
    new SandboxxingMicroVMStack(app, 'SandboxxingMicroVM', {
      description: 'AWS resources of the sandboxxing microvm backend',
    });
    break;
  case 'ec2':
    new SandboxxingEC2Stack(app, 'SandboxxingEC2', {
      description: 'AWS resources of the sandboxxing EC2 backend',
      ...(String(app.node.tryGetContext('useDefaultVpc')) === 'true'
        ? { env: { account: process.env.CDK_DEFAULT_ACCOUNT, region: process.env.CDK_DEFAULT_REGION } }
        : {}),
    });
    break;
  default:
    throw new Error('backend must be microvm or ec2');
}
