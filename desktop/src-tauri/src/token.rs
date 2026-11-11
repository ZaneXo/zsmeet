//! App 令牌放在哪里：正式运行时放 macOS 钥匙串，测试时放内存（测试不能碰真的钥匙串）。
//!
//! 每台服务器一条：钥匙串里的「服务」是 App 的 identifier，「账户」是服务器地址。

#[cfg(test)]
use std::collections::HashMap;
#[cfg(test)]
use std::sync::Mutex;

pub trait TokenStore: Send + Sync {
    /// 读出这台服务器的令牌；没有存过时返回 None。
    fn get(&self, server: &str) -> Result<Option<String>, String>;
    fn set(&self, server: &str, token: &str) -> Result<(), String>;
    /// 删掉这台服务器的令牌；本来就没有也算成功。
    fn delete(&self, server: &str) -> Result<(), String>;
}

/// 放在内存里，只给测试用。
#[cfg(test)]
#[derive(Default)]
pub struct MemoryStore {
    items: Mutex<HashMap<String, String>>,
}

#[cfg(test)]
impl TokenStore for MemoryStore {
    fn get(&self, server: &str) -> Result<Option<String>, String> {
        Ok(self.items.lock().unwrap().get(server).cloned())
    }

    fn set(&self, server: &str, token: &str) -> Result<(), String> {
        self.items
            .lock()
            .unwrap()
            .insert(server.to_string(), token.to_string());
        Ok(())
    }

    fn delete(&self, server: &str) -> Result<(), String> {
        self.items.lock().unwrap().remove(server);
        Ok(())
    }
}

/// macOS 钥匙串里的「通用密码」。
#[cfg(target_os = "macos")]
pub struct KeychainStore {
    service: String,
}

#[cfg(target_os = "macos")]
impl KeychainStore {
    pub fn new(service: &str) -> Self {
        Self {
            service: service.to_string(),
        }
    }
}

/// 钥匙串里没有这一项（errSecItemNotFound）。
#[cfg(target_os = "macos")]
const ITEM_NOT_FOUND: i32 = -25300;

#[cfg(target_os = "macos")]
impl TokenStore for KeychainStore {
    fn get(&self, server: &str) -> Result<Option<String>, String> {
        use security_framework::passwords::get_generic_password;
        match get_generic_password(&self.service, server) {
            Ok(bytes) => Ok(String::from_utf8(bytes).ok()),
            Err(e) if e.code() == ITEM_NOT_FOUND => Ok(None),
            Err(e) => Err(format!(
                "读不了钥匙串里保存的登录信息（{e}），请在弹出的提示里点「允许」，或者重新登录"
            )),
        }
    }

    fn set(&self, server: &str, token: &str) -> Result<(), String> {
        use security_framework::passwords::set_generic_password;
        set_generic_password(&self.service, server, token.as_bytes())
            .map_err(|e| format!("登录信息没能存进钥匙串（{e}），请重新登录"))
    }

    fn delete(&self, server: &str) -> Result<(), String> {
        use security_framework::passwords::delete_generic_password;
        match delete_generic_password(&self.service, server) {
            Ok(()) => Ok(()),
            Err(e) if e.code() == ITEM_NOT_FOUND => Ok(()),
            Err(e) => Err(format!("没能从钥匙串里删掉登录信息（{e}）")),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn memory_store_round_trip() {
        let store = MemoryStore::default();
        assert_eq!(store.get("https://a").unwrap(), None);
        store.set("https://a", "zsm_1").unwrap();
        store.set("https://b", "zsm_2").unwrap();
        assert_eq!(store.get("https://a").unwrap().as_deref(), Some("zsm_1"));
        store.delete("https://a").unwrap();
        store.delete("https://a").unwrap();
        assert_eq!(store.get("https://a").unwrap(), None);
        assert_eq!(store.get("https://b").unwrap().as_deref(), Some("zsm_2"));
    }
}
