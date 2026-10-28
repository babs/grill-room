import * as provider from './provider.js';
import { db } from './playwright.config.js';
import { restricted } from '../oidc/scenarios.js';

restricted(provider, db);
