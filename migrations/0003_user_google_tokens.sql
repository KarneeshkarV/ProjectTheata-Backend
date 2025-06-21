-- +goose Up
-- +goose StatementBegin
/*CREATE TABLE IF NOT EXISTS user_google_tokens (*/
    /*id UUID PRIMARY KEY DEFAULT gen_random_uuid(),*/
    /*user_id UUID NOT NULL UNIQUE REFERENCES auth.users(id) ON DELETE CASCADE,*/
    /*encrypted_access_token TEXT NOT NULL,*/
    /*encrypted_refresh_token TEXT, -- Refresh token might not always be present or change*/
    /*token_expiry TIMESTAMPTZ,*/
    /*scopes TEXT[],*/
    /*created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),*/
    /*updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()*/
/*);*/

/*-- Trigger to automatically update 'updated_at' timestamp*/
/*CREATE OR REPLACE FUNCTION trigger_set_timestamp()*/
/*RETURNS TRIGGER AS $$*/
/*BEGIN*/
  /*NEW.updated_at = NOW();*/
  /*RETURN NEW;*/
/*END;*/
/*$$ LANGUAGE plpgsql;*/

/*CREATE TRIGGER set_user_google_tokens_updated_at*/
/*BEFORE UPDATE ON user_google_tokens*/
/*FOR EACH ROW*/
/*EXECUTE FUNCTION trigger_set_timestamp();*/

/*CREATE INDEX IF NOT EXISTS idx_user_google_tokens_user_id ON user_google_tokens(user_id);*/
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
/*DROP TRIGGER IF EXISTS set_user_google_tokens_updated_at ON user_google_tokens;*/
/*DROP FUNCTION IF EXISTS trigger_set_timestamp(); -- Be cautious if this function is used by other tables*/
/*DROP TABLE IF EXISTS user_google_tokens;*/
-- +goose StatementEnd
