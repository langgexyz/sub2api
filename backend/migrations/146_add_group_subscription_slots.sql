-- 订阅型 group 的固定槽位数 N：绑定号容量按 N 等分给各订阅。默认 1（单订阅独占，= 现有行为）。
ALTER TABLE groups ADD COLUMN IF NOT EXISTS subscription_slots integer NOT NULL DEFAULT 1;
