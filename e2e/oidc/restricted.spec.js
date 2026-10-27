import * as provider from './provider.js';
import { db } from './playwright.config.js';
import { restricted } from './scenarios.js';

restricted(provider, db);
