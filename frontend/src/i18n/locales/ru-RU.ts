import common from './ru-RU/common.ts'
import auth from './ru-RU/auth.ts'
import chat from './ru-RU/chat.ts'
import agent from './ru-RU/agent.ts'
import knowledge from './ru-RU/knowledge.ts'
import datasource from './ru-RU/datasource.ts'
import model from './ru-RU/model.ts'
import settings from './ru-RU/settings.ts'
import system from './ru-RU/system.ts'

import { feedbackAdminEN } from '../feedbackAdminMessages'
export default {
  feedbackAdmin: feedbackAdminEN,
  ...common,
  ...auth,
  ...chat,
  ...agent,
  ...knowledge,
  ...datasource,
  ...model,
  ...settings,
  ...system,
}
