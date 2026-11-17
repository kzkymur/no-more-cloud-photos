import { requireNativeModule } from 'expo';

import type { NmcpUploaderApi } from './NmcpUploader.types';

export default requireNativeModule<NmcpUploaderApi>('NmcpUploader');
export * from './NmcpUploader.types';
